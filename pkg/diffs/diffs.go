package diffs

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/ghodss/yaml"
	"k8s.io/apimachinery/pkg/api/equality"
	prowconfig "k8s.io/test-infra/prow/config"
)

// PreSubmits holds the logic of prow's presubmits yaml format.
type PreSubmits struct {
	PreSubmits map[string][]prowconfig.Presubmit `yaml:"presubmits,omitempty"`
}

// Diffs holds the logic about the differences between the files.
type Diffs struct {
	JobConfigPath  string
	CiOpConfigPath string
	DecorationPath string
}

// NewDiffs creates a new Diffs{}
func NewDiffs(jobConfigPath, ciOpConfigPath, decorationPath string) *Diffs {
	return &Diffs{
		JobConfigPath:  jobConfigPath,
		CiOpConfigPath: ciOpConfigPath,
		DecorationPath: decorationPath,
	}

}

func (d *Diffs) findChangedFiles() ([]string, error) {
	var changedFiles []string
	// Walk through the cloned repo
	if err := filepath.Walk(path.Join("ci-operator/jobs/", d.DecorationPath), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			log.Printf("prevent panic by handling failure accessing a path %q: %v\n", path, err)
			return err
		}

		if !info.IsDir() && filepath.Ext(path) == ".yaml" {
			masterFileContents, err := ioutil.ReadFile(filepath.Join(d.JobConfigPath, filepath.Base(path)))
			if err != nil {
				log.Printf("Can't read master's file: %v", err)
			}
			prFileContents, err := ioutil.ReadFile(path)
			if err != nil {
				log.Printf("Error reading file %s: %v", path, err)
			}
			if bytes.Compare(masterFileContents, prFileContents) != 0 {
				changedFiles = append(changedFiles, path)
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return changedFiles, nil
}

func (d *Diffs) getPresubmitChanges() []string {
	var preSubmitChangedFiles []string

	changedFiles, err := d.findChangedFiles()
	if err != nil {
		log.Fatal("Couldn't get changed files.")
	}

	for _, changedFile := range changedFiles {
		if strings.Contains(filepath.Base(changedFile), "presubmits.yaml") {
			preSubmitChangedFiles = append(preSubmitChangedFiles, changedFile)
		}
	}
	return preSubmitChangedFiles
}

// We return a map of maps here to make the comparing more efficient.
func getPresubmits(file string) (map[string]map[string]prowconfig.Presubmit, error) {
	var preSubmits PreSubmits

	jobsByRepo := make(map[string]map[string]prowconfig.Presubmit)

	data, err := ioutil.ReadFile(file)
	if err != nil {
		return jobsByRepo, fmt.Errorf("failed to read presubmit file (%v)", err)
	}

	if err := yaml.Unmarshal(data, &preSubmits); err != nil {
		return jobsByRepo, fmt.Errorf("failed to unmarshal presubmit file (%v)", err)
	}

	for repo, preSubmitList := range preSubmits.PreSubmits {
		pm := make(map[string]prowconfig.Presubmit)
		for _, p := range preSubmitList {
			pm[p.Name] = p
			jobsByRepo[repo] = pm
		}
	}
	return jobsByRepo, nil
}

// GetPresubmitsToExecute returns a mapping of repo to presubmits to execute.
func (d *Diffs) GetPresubmitsToExecute() map[string][]prowconfig.Presubmit {
	var preSubmitsToExecute []prowconfig.Presubmit
	preSubmitsToExecuteMap := make(map[string][]prowconfig.Presubmit)

	for _, preSubmitChangedFile := range d.getPresubmitChanges() {
		prPreSubmits, err := getPresubmits(preSubmitChangedFile)
		if err != nil {
			log.Fatal(err)
		}
		masterPreSubmits, _ := getPresubmits(fmt.Sprintf("%s/%s", d.JobConfigPath, filepath.Base(preSubmitChangedFile)))

		for repo, jobs := range prPreSubmits {
			preSubmitsToExecute = []prowconfig.Presubmit{}
			for jobName, job := range jobs {
				if !equality.Semantic.DeepEqual(masterPreSubmits[repo][jobName].Spec, job.Spec) {
					preSubmitsToExecute = append(preSubmitsToExecute, job)
				}
			}

			// The same repo can contain presubmits from different branches as well.
			// In this case, just append them.
			if _, ok := preSubmitsToExecuteMap[repo]; ok {
				preSubmitsToExecuteMap[repo] = append(preSubmitsToExecuteMap[repo], preSubmitsToExecute...)
			} else {
				preSubmitsToExecuteMap[repo] = preSubmitsToExecute
			}
		}
	}
	return preSubmitsToExecuteMap
}
