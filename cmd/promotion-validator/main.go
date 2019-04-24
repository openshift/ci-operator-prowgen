package main

import (
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/sirupsen/logrus"

	"github.com/openshift/ci-operator/pkg/api"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/yaml"

	"github.com/openshift/ci-operator-prowgen/pkg/config"
	"github.com/openshift/ci-operator-prowgen/pkg/diffs"
	"github.com/openshift/ci-operator-prowgen/pkg/promotion"
)

type options struct {
	targetRelease       string
	latestRelease       bool
	releaseRepoDir      string
	ocpBuildDataRepoDir string

	logLevel string
}

func (o *options) Validate() error {
	if o.releaseRepoDir == "" {
		return errors.New("required flag --release-repo-dir was unset")
	}

	if o.ocpBuildDataRepoDir == "" {
		return errors.New("required flag --ocp-build-data-repo-dir was unset")
	}

	if o.targetRelease == "" {
		return errors.New("required flag --target-release was unset")
	}

	level, err := logrus.ParseLevel(o.logLevel)
	if err != nil {
		return fmt.Errorf("invalid --log-level: %v", err)
	}
	logrus.SetLevel(level)
	return nil
}

func (o *options) Bind(fs *flag.FlagSet) {
	fs.StringVar(&o.targetRelease, "target-release", "", "Configurations targeting this release will get validated.")
	fs.BoolVar(&o.latestRelease, "latest-release", false, "The release targeted has development branches promoting to it.")
	fs.StringVar(&o.releaseRepoDir, "release-repo-dir", "", "Path to openshift/release repo.")
	fs.StringVar(&o.ocpBuildDataRepoDir, "ocp-build-data-repo-dir", "", "Path to openshift/ocp-build-data repo.")
	fs.StringVar(&o.logLevel, "log-level", "info", "Level at which to log output.")
}

func gatherOptions() options {
	o := options{}
	fs := flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	o.Bind(fs)
	if err := fs.Parse(os.Args[1:]); err != nil {
		logrus.WithError(err).Fatal("could not parse input")
	}
	return o
}

func main() {
	o := gatherOptions()
	if err := o.Validate(); err != nil {
		logrus.Fatalf("Invalid options: %v", err)
	}

	raw, err := ioutil.ReadFile(filepath.Join(o.ocpBuildDataRepoDir, "group.yml"))
	if err != nil {
		logrus.WithError(err).Fatal("Could not load OCP build data branch configuration.")
	}

	var groupConfig branchConfig
	if err := yaml.Unmarshal(raw, &groupConfig); err != nil {
		logrus.WithError(err).Fatal("Could not unmarshal OCP build data branch configuration.")
	}
	fmt.Println(groupConfig)
	targetRelease := fmt.Sprintf("%d.%d", groupConfig.Vars.Major, groupConfig.Vars.Minor)
	if expected, actual := targetRelease, o.targetRelease; expected != actual {
		logrus.Fatalf("Release configured in OCP build data (%s) does not match that in CI (%s)", expected, actual)
	}

	imageConfigByName := map[string]imageConfig{}
	if err := filepath.Walk(filepath.Join(o.ocpBuildDataRepoDir, "images"), func(path string, info os.FileInfo, err error) error {
		if info.IsDir() {
			return nil
		}

		// we know the path is relative, but there is no API to declare that
		relPath, _ := filepath.Rel(o.ocpBuildDataRepoDir, path)
		logger := logrus.WithField("source-file", relPath)
		raw, err := ioutil.ReadFile(path)
		if err != nil {
			logger.WithError(err).Fatal("Could not load OCP build data configuration.")
		}

		var productConfig imageConfig
		if err := yaml.Unmarshal(raw, &productConfig); err != nil {
			logger.WithError(err).Fatal("Could not unmarshal OCP build data configuration.")
		}
		productConfig.path = relPath

		imageConfigByName[productConfig.Name] = productConfig
		return nil
	}); err != nil {
		logrus.WithError(err).Fatal("Could walk OCP build data configuration directory.")
	}

	var foundFailures bool
	if err := config.OperateOnCIOperatorConfigDir(path.Join(o.releaseRepoDir, diffs.CIOperatorConfigInRepoPath), func(configuration *api.ReleaseBuildConfiguration, info *config.Info) error {
		if !(promotion.BuildOfficialImages(configuration) && configuration.PromotionConfiguration.Name == o.targetRelease) {
			return nil
		}
		logger := config.LoggerForInfo(*info)

		if info.Org == "openshift" && info.Repo == "origin" {
			// a couple of things are special here -- we will have ocp-build-data config only for
			// ose repo and only for branches prefixed with enterprise-, not release-
			info.Repo = "ose"
			info.Branch = strings.Replace(info.Branch, "release", "enterprise", 1)
		}

		for _, image := range configuration.Images {
			if image.Optional {
				continue
			}
			logger = logger.WithField("image", image.To)
			imageName := productImageName(string(image.To))
			logger.Debug("Validating image.")
			if sets.NewString(groupConfig.NonRelease.Images...).HasAny(imageName, string(image.To)) {
				logger.Warnf("Promotion found in CI for image %s, but publication is disabled in OCP build data.", image.To)
				continue
			}
			productConfig, exists := imageConfigByName[imageName]
			if !exists {
				logger.Errorf("Promotion found in CI for image %s, but no configuration for %s found in OCP build data.", image.To, imageName)
				continue
			}
			logger = logger.WithField("ocp-build-data-path", productConfig.path)

			var source git
			alias := productConfig.Content.Source.Alias
			if alias != "" {
				aliasedSource, ok := groupConfig.Sources[alias]
				if !ok {
					logger.Errorf("Alias %s not found in group configuration.", alias)
					foundFailures = true
				}
				source = aliasedSource
			} else {
				literalSource := productConfig.Content.Source.Git
				if reflect.DeepEqual(literalSource, new(git)) {
					logger.Error("No alias or source found in configuration.")
					foundFailures = true
				}
				source = literalSource
			}

			validateTarget := func() {
				resolvedBranch := strings.Replace(source.Branch.Target, "{MAJOR}.{MINOR}", targetRelease, -1)
				if actual, expected := info.Branch, resolvedBranch; actual != expected {
					if expected == "" {
						logger.Error("Target branch not set in OCP build data configuration.")
					} else {
						logger.Errorf("Target branch in CI Operator configuration (%s) does not match that resolved from OCP build data (%s).", actual, expected)
					}
					foundFailures = true
				}
			}

			validateFallback := func() {
				if actual, expected := info.Branch, source.Branch.Fallback; actual != expected {
					if expected == "" {
						logger.Error("Fallback branch not set in OCP build data configuration.")
					} else {
						logger.Errorf("Fallback branch in CI Operator configuration (%s) does not match that from OCP build data (%s).", actual, expected)
					}
					foundFailures = true
				}
			}
			if o.latestRelease {
				// CI will build out of dev branches and have disabled promotion from
				// release branches until things cut over, so we should check both
				if promotion.IsDisabled(configuration) {
					validateTarget()
				} else {
					validateFallback()
				}
			} else {
				// we only have the simple case
				validateTarget()
			}

			// there is no standard, we just need to generally point at the right thing
			urls := []string{
				fmt.Sprintf("git@github.com:%s/%s", info.Org, info.Repo),
				fmt.Sprintf("git@github.com:%s/%s.git", info.Org, info.Repo),
				fmt.Sprintf("https://github.com/%s/%s", info.Org, info.Repo),
				fmt.Sprintf("https://github.com/%s/%s.git", info.Org, info.Repo),
			}
			if actual, expected := source.Url, sets.NewString(urls...); !expected.Has(actual) {
				if actual == "" {
					logger.Error("Source repo URL not set in OCP build data configuration.")
				} else {
					logger.Errorf("Source repo URL in OCP build data (%s) is not a recognized URL for %s/%s.", actual, info.Org, info.Repo)
				}
				foundFailures = true
			}
		}
		return nil
	}); err != nil {
		logrus.WithError(err).Fatal("Could not load CI Operator configurations.")
	}

	if foundFailures {
		logrus.Fatal("Found configurations that promote to official streams but do not have corresponding OCP build data configurations.")
	}
}

// productImageName determines the image name in OSBS for an image
// from CI. This is a combination of convention and hacks
func productImageName(name string) string {
	switch name {
	case "ansible":
		return "openshift/openshift-ansible"
	default:
		return fmt.Sprintf("openshift/ose-%s", name)
	}
}

// branchConfig holds branch-wide configurations in the ocp-build-data repository
type branchConfig struct {
	Vars       vars           `json:"vars"`
	Sources    map[string]git `json:"sources"`
	NonRelease nonRelease     `json:"non_release"`
}

// nonRelease holds blacklists for building
type nonRelease struct {
	Images []string `json:"images"`
}

type vars struct {
	Major int `json:"MAJOR"`
	Minor int `json:"MINOR"`
}

// imageConfig is the configuration stored in the ocp-build-data repository
type imageConfig struct {
	Content content `json:"content"`
	Name    string  `json:"name"`

	// added by us
	path string
}

type content struct {
	Source source `json:"source"`
}

type source struct {
	Alias string `json:"alias"`
	Git   git    `json:"git"`
}

type git struct {
	Branch branch `json:"branch"`
	Url    string `json:"url"`
}

type branch struct {
	Target   string `json:"target,omitempty"`
	Fallback string `json:"fallback,omitempty"`
}
