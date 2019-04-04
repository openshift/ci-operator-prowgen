package main

import (
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"strings"

	"github.com/sirupsen/logrus"
	"k8s.io/test-infra/prow/github"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/test-infra/prow/config/org"
	"sigs.k8s.io/yaml"
)

type options struct {
	membersPath string
	reposPath   string
	configPath  string
	logLevel    string
}

func (o *options) Validate() error {
	if o.membersPath == "" {
		return errors.New("required flag --members was unset")
	}

	if o.reposPath == "" {
		return errors.New("required flag --repos was unset")
	}

	if o.configPath == "" {
		return errors.New("required flag --config was unset")
	}

	level, err := logrus.ParseLevel(o.logLevel)
	if err != nil {
		return fmt.Errorf("invalid --log-level: %v", err)
	}
	logrus.SetLevel(level)
	return nil
}

func gatherOptions() options {
	o := options{}
	fs := flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	fs.StringVar(&o.membersPath, "members", "", "Path to AOS Team Member Tracking spreadsheet.")
	fs.StringVar(&o.reposPath, "repos", "", "Path to AOS Repository Tracking spreadsheet.")
	fs.StringVar(&o.configPath, "config", "", "Path to peribolos config to update.")
	fs.StringVar(&o.logLevel, "log-level", "info", "Level at which to log output.")
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

	var orgConfig struct {
		Orgs map[string]org.Config `json:"orgs,omitempty"`
	}
	rawConfig, err := ioutil.ReadFile(o.configPath)
	if err != nil {
		logrus.WithError(err).Fatal("Failed to load peribolos org config.")
	}

	if err := yaml.Unmarshal(rawConfig, &orgConfig); err != nil {
		logrus.WithError(err).Fatal("Failed to unmarshal peribolos org config.")
	}

	rawRepos, err := os.Open(o.reposPath)
	if err != nil {
		logrus.WithError(err).Fatal("Failed to load repos spreadsheet.")
	}
	defer func() {
		if err := rawRepos.Close(); err != nil {
			logrus.WithError(err).Fatal("Failed to close repos spreadsheet.")
		}
	}()

	repoRecords, err := csv.NewReader(rawRepos).ReadAll()
	if err != nil {
		logrus.WithError(err).Fatal("Failed to parse repos spreadsheet.")
	}

	reposByTeam := map[string]sets.String{}
	for _, row := range repoRecords {
		repos := sets.NewString()
		for _, repo := range row[1:] {
			if repo != "" && strings.HasPrefix(repo, "openshift/") {
				repos.Insert(strings.TrimSpace(strings.TrimPrefix(repo, "openshift/")))
			}
		}
		reposByTeam[row[0]] = repos
	}

	rawMembers, err := os.Open(o.membersPath)
	if err != nil {
		logrus.WithError(err).Fatal("Failed to load members spreadsheet.")
	}
	defer func() {
		if err := rawMembers.Close(); err != nil {
			logrus.WithError(err).Fatal("Failed to close members spreadsheet.")
		}
	}()

	memberRecords, err := csv.NewReader(rawMembers).ReadAll()
	if err != nil {
		logrus.WithError(err).Fatal("Failed to parse members spreadsheet.")
	}

	var members []teamMember
	// ignore the header
	for _, row := range memberRecords[1:] {
		member := teamMemberFromRow(row)
		if member.name == "" {
			continue
		}
		if member.githubID == "" {
			logrus.Warnf("No GitHub ID found for %s <%s>", member.name, member.email)
			continue
		}
		members = append(members, member)
	}

	insertID := func(records map[string]sets.String, key, id string) {
		if records[key] == nil {
			records[key] = sets.NewString(id)
		} else {
			records[key].Insert(id)
		}
	}
	githubIDsbyGroup := map[string]sets.String{}
	githubIDsbyTeam := map[string]sets.String{}
	maintainersByTeam := map[string]sets.String{}
	githubIDsbyRole := map[string]sets.String{}
	allGithubIDs := sets.NewString(orgConfig.Orgs["openshift"].Members...)
	for _, member := range members {
		allGithubIDs.Insert(member.githubID)
		if member.group != "" {
			insertID(githubIDsbyGroup, member.group, member.githubID)
		}

		if member.team != "" && member.team != "OpenShift Architect" && member.team != "Group Lead" {
			insertID(githubIDsbyTeam, member.team, member.githubID)
			// maintainer teams are needed for granular GitHub permissions, leads are maintainers
			if member.roles.HasAny(roleTeamLead, roleMaintainer) {
				insertID(maintainersByTeam, member.team, member.githubID)
			}
		}
		// only a couple roles are special
		for _, role := range member.roles.List() {
			if sets.NewString(roleArchitect, roleGroupLead, roleTeamLead).Has(role) {
				insertID(githubIDsbyRole, role, member.githubID)
			}
		}
	}

	closed := org.Closed
	updateTeamMembers := func(name, description string, members []string, repos map[string]github.RepoPermissionLevel) {
		team, exists := orgConfig.Orgs["openshift"].Teams[name]
		if !exists {
			team = org.Team{
				TeamMetadata: org.TeamMetadata{
					Description: &description,
					Privacy:     &closed,
				},
				Maintainers: []string{"openshift-ci-robot"},
			}
		}
		team.Members = members
		team.Repos = repos
		orgConfig.Orgs["openshift"].Teams[name] = team
	}

	for group, ids := range githubIDsbyGroup {
		name := fmt.Sprintf("OpenShift Group %s", group)
		description := fmt.Sprintf("Members of the OpenShift development group %s", group)
		updateTeamMembers(name, description, ids.List(), map[string]github.RepoPermissionLevel{})
	}

	for team, ids := range githubIDsbyTeam {
		name := fmt.Sprintf("OpenShift Team %s", team)
		description := fmt.Sprintf("Members of the OpenShift development team %s", team)
		permissions := map[string]github.RepoPermissionLevel{}
		for _, repo := range reposByTeam[team].List() {
			permissions[repo] = github.Read
		}
		updateTeamMembers(name, description, ids.List(), permissions)
	}

	for team, ids := range maintainersByTeam {
		name := fmt.Sprintf("OpenShift Team %s Maintainers", team)
		description := fmt.Sprintf("Members of the OpenShift development team %s maintainers", team)
		permissions := map[string]github.RepoPermissionLevel{}
		for _, repo := range reposByTeam[team].List() {
			permissions[repo] = github.Write
		}
		updateTeamMembers(name, description, ids.List(), permissions)
	}

	for role, ids := range githubIDsbyRole {
		name := fmt.Sprintf("OpenShift %ss", role)
		description := name
		permissions := map[string]github.RepoPermissionLevel{}
		for _, repos := range reposByTeam {
			for _, repo := range repos.List() {
				switch role {
				case roleArchitect:
					permissions[repo] = github.Admin
				case roleGroupLead:
					permissions[repo] = github.Write
				}
			}
		}
		updateTeamMembers(name, description, ids.List(), permissions)
	}

	org := orgConfig.Orgs["openshift"]
	org.Members = allGithubIDs.Difference(sets.NewString(org.Admins...)).List()
	orgConfig.Orgs["openshift"] = org

	edited, err := yaml.Marshal(orgConfig)
	if err != nil {
		logrus.WithError(err).Fatal("Failed to marshal edited org config.")
	}

	if err := ioutil.WriteFile(o.configPath, edited, 0666); err != nil {
		logrus.WithError(err).Fatal("Failed to write edited org config.")
	}
	logrus.Info("Updated org config using spreadsheet data.")
}

// teamMemberFromRow parses a spreadsheet row into a record; columns are:
//  0: group
//  1: team
//  2: time
//  3: name
//  4: time zone
//  5: roles
//  6: notes
//  7: valid team
//  8: email
//  9: github id
func teamMemberFromRow(row []string) teamMember {
	roles := sets.NewString()
	for _, role := range strings.Split(row[5], ",") {
		roles.Insert(strings.TrimSpace(role))
	}

	return teamMember{
		group:    row[0],
		team:     row[1],
		roles:    roles,
		name:     row[3],
		email:    row[8],
		githubID: row[9],
	}
}

type teamMember struct {
	group    string
	team     string
	roles    sets.String
	name     string
	email    string
	githubID string
}

const (
	roleArchitect  = "Architect"
	roleGroupLead  = "Group Lead"
	roleTeamLead   = "Team Lead"
	roleMaintainer = "Maintainer"
)
