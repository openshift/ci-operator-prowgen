package main

import (
	"flag"
	"fmt"
	"log"
	"net/url"

	"github.com/openshift/ci-operator-prowgen/pkg/diffs"
)

type options struct {
	ciOpConfigPath  string
	jobConfigPath   string
	decorationPath  string
	githubEndpoint  string
	githubTokenFile string
}

func gatherOptions() options {
	o := options{}
	flag.StringVar(&o.githubEndpoint, "github-endpoint", "https://api.github.com", "GitHub's API endpoint.")
	flag.StringVar(&o.githubTokenFile, "github-token-file", "", "Path to file containing GitHub OAuth token.")
	flag.StringVar(&o.ciOpConfigPath, "ci-op-config-path", "", "Path to ci-operator's configuration files..")
	flag.StringVar(&o.jobConfigPath, "job-config-path", "", "Path to prow job configs.")
	flag.StringVar(&o.decorationPath, "decoration-path", "./", "Path where the repository has been cloned.")
	flag.Parse()
	return o
}

func validateOptions(o options) error {
	if len(o.githubTokenFile) == 0 {
		return fmt.Errorf("empty --github-token-file")
	}

	if len(o.githubEndpoint) == 0 {
		return fmt.Errorf("empty --github-endpoint")
	} else if _, err := url.Parse(o.githubEndpoint); err != nil {
		return fmt.Errorf("bad --github-endpoint provided: %v", err)
	}

	if len(o.ciOpConfigPath) == 0 {
		return fmt.Errorf("empty --ci-op-config-path")
	}

	if len(o.jobConfigPath) == 0 {
		return fmt.Errorf("empty --job-config-path")
	}

	return nil
}

func main() {
	o := gatherOptions()
	err := validateOptions(o)
	if err != nil {
		log.Fatal(err)
	}

	diffs := diffs.NewDiffs(o.jobConfigPath, o.ciOpConfigPath, o.decorationPath)
	//diffs.GetPresubmitsToExecute()
	preSubmitsToExecute := diffs.GetPresubmitsToExecute()

	// // Just print the map with the presubmits to be executed.
	// // TODO: execute them
	for k, v := range preSubmitsToExecute {
		log.Printf("############### %s ###############:", k)
		for _, p := range v {
			log.Printf("%s", p.Name)
		}
	}
}
