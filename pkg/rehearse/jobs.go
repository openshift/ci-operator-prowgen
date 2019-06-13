package rehearse

import (
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/getlantern/deepcopy"
	"github.com/ghodss/yaml"
	"github.com/sirupsen/logrus"

	"k8s.io/api/core/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"

	"k8s.io/client-go/kubernetes/fake"
	coreclientset "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	coretesting "k8s.io/client-go/testing"

	pjapi "k8s.io/test-infra/prow/apis/prowjobs/v1"
	pjclientset "k8s.io/test-infra/prow/client/clientset/versioned"
	pjclientsetfake "k8s.io/test-infra/prow/client/clientset/versioned/fake"
	pj "k8s.io/test-infra/prow/client/clientset/versioned/typed/prowjobs/v1"
	prowconfig "k8s.io/test-infra/prow/config"
	"k8s.io/test-infra/prow/pjutil"

	"github.com/openshift/ci-operator-prowgen/pkg/config"
)

const (
	rehearseLabel                = "ci.openshift.org/rehearse"
	defaultRehearsalRerunCommand = "/test pj-rehearse"
	logRehearsalJob              = "rehearsal-job"
	logCiopConfigFile            = "ciop-config-file"
	logCiopConfigRepo            = "ciop-config-repo"

	clusterTypeEnvName = "CLUSTER_TYPE"
)

// Loggers holds the two loggers that will be used for normal and debug logging respectively.
type Loggers struct {
	Job, Debug logrus.FieldLogger
}

// NewProwJobClient creates a ProwJob client with a dry run capability
func NewProwJobClient(clusterConfig *rest.Config, namespace string, dry bool) (pj.ProwJobInterface, error) {
	if dry {
		pjcset := pjclientsetfake.NewSimpleClientset()
		return pjcset.ProwV1().ProwJobs(namespace), nil
	}
	pjcset, err := pjclientset.NewForConfig(clusterConfig)
	if err != nil {
		return nil, err
	}
	return pjcset.ProwV1().ProwJobs(namespace), nil
}

// NewCMClient creates a configMap client with a dry run capability
func NewCMClient(clusterConfig *rest.Config, namespace string, dry bool) (coreclientset.ConfigMapInterface, error) {
	if dry {
		c := fake.NewSimpleClientset()
		c.PrependReactor("update", "configmaps", func(action coretesting.Action) (bool, runtime.Object, error) {
			cm := action.(coretesting.UpdateAction).GetObject().(*v1.ConfigMap)
			y, err := yaml.Marshal([]*v1.ConfigMap{cm})
			if err != nil {
				return true, nil, fmt.Errorf("failed to convert ConfigMap to YAML: %v", err)
			}
			fmt.Print(string(y))
			return false, nil, nil
		})
		return c.CoreV1().ConfigMaps(namespace), nil
	}

	cmClient, err := coreclientset.NewForConfig(clusterConfig)
	if err != nil {
		return nil, fmt.Errorf("could not get core client for cluster config: %v", err)
	}

	return cmClient.ConfigMaps(namespace), nil
}

func makeRehearsalPresubmit(source *prowconfig.Presubmit, repo string, prNumber int) (*prowconfig.Presubmit, error) {
	var rehearsal prowconfig.Presubmit
	deepcopy.Copy(&rehearsal, source)

	rehearsal.Name = fmt.Sprintf("rehearse-%d-%s", prNumber, source.Name)

	branch := strings.TrimPrefix(strings.TrimSuffix(source.Branches[0], "$"), "^")
	shortName := strings.TrimPrefix(source.Context, "ci/prow/")
	rehearsal.Context = fmt.Sprintf("ci/rehearse/%s/%s/%s", repo, branch, shortName)
	rehearsal.RerunCommand = defaultRehearsalRerunCommand

	gitrefArg := fmt.Sprintf("--git-ref=%s@%s", repo, branch)
	rehearsal.Spec.Containers[0].Args = append(source.Spec.Containers[0].Args, gitrefArg)
	rehearsal.Optional = true

	if rehearsal.Labels == nil {
		rehearsal.Labels = make(map[string]string, 1)
	}
	rehearsal.Labels[rehearseLabel] = strconv.Itoa(prNumber)

	return &rehearsal, nil
}

func makeRehearsalPeriodic(source *prowconfig.Periodic, prNumber int) (prowconfig.Periodic, error) {
	var rehearsal prowconfig.Periodic
	deepcopy.Copy(&rehearsal, source)

	rehearsal.Name = fmt.Sprintf("rehearse-%d-%s", prNumber, source.Name)
	if rehearsal.Labels == nil {
		rehearsal.Labels = make(map[string]string, 1)
	}
	rehearsal.Labels[rehearseLabel] = strconv.Itoa(prNumber)

	return rehearsal, nil
}

func filterJobs(changedPresubmits map[string][]prowconfig.Presubmit, changedPeriodics []prowconfig.Periodic, allowVolumes bool, logger logrus.FieldLogger) (config.Presubmits, []prowconfig.Periodic) {
	presubmits := config.Presubmits{}
	var periodics []prowconfig.Periodic
	for repo, jobs := range changedPresubmits {
		for _, job := range jobs {
			jobLogger := logger.WithFields(logrus.Fields{"repo": repo, "job": job.Name})
			if len(job.Branches) == 0 {
				jobLogger.Warn("cannot rehearse jobs with no branches")
				continue
			}

			if len(job.Branches) != 1 {
				jobLogger.Warn("cannot rehearse jobs that run over multiple branches")
				continue
			}

			if err := filterJob(job.Spec, allowVolumes); err != nil {
				jobLogger.WithError(err).Warn("could not rehearse job")
				continue
			}
			presubmits.Add(repo, job)
		}
	}

	for _, periodic := range changedPeriodics {
		jobLogger := logger.WithField("job", periodic.Name)
		if err := filterJob(periodic.Spec, allowVolumes); err != nil {
			jobLogger.WithError(err).Warn("could not rehearse job")
			continue
		}

		periodics = append(periodics, periodic)
	}

	return presubmits, periodics
}

func filterJob(spec *v1.PodSpec, allowVolumes bool) error {
	// there will always be exactly one container.
	container := spec.Containers[0]

	if len(container.Command) != 1 || container.Command[0] != "ci-operator" {
		return fmt.Errorf("cannot rehearse jobs that have Command different from simple 'ci-operator'")
	}

	for _, arg := range container.Args {
		if strings.HasPrefix(arg, "--git-ref") || strings.HasPrefix(arg, "-git-ref") {
			return fmt.Errorf("cannot rehearse jobs that call ci-operator with '--git-ref' arg")
		}
	}
	if len(spec.Volumes) > 0 && !allowVolumes {
		return fmt.Errorf("jobs that need additional volumes mounted are not allowed")
	}

	return nil
}

// inlineCiOpConfig detects whether a job needs a ci-operator config file
// provided by a `ci-operator-configs` ConfigMap and if yes, returns a copy
// of the job where a reference to this ConfigMap is replaced by the content
// of the needed config file passed to the job as a direct value. This needs
// to happen because the rehearsed Prow jobs may depend on these config files
// being also changed by the tested PR.
func inlineCiOpConfig(container v1.Container, ciopConfigs config.CompoundCiopConfig, loggers Loggers) error {
	for index := range container.Env {
		env := &(container.Env[index])
		if env.ValueFrom == nil {
			continue
		}
		if env.ValueFrom.ConfigMapKeyRef == nil {
			continue
		}
		if config.IsCiopConfigCM(env.ValueFrom.ConfigMapKeyRef.Name) {
			filename := env.ValueFrom.ConfigMapKeyRef.Key

			loggers.Debug.WithField(logCiopConfigFile, filename).Debug("Rehearsal job uses ci-operator config ConfigMap, needed content will be inlined")

			ciopConfig, ok := ciopConfigs[filename]
			if !ok {
				return fmt.Errorf("ci-operator config file %s was not found", filename)
			}

			ciOpConfigContent, err := yaml.Marshal(ciopConfig)
			if err != nil {
				loggers.Job.WithError(err).Error("Failed to marshal ci-operator config file")
				return err
			}

			env.Value = string(ciOpConfigContent)
			env.ValueFrom = nil
		}
	}
	return nil
}

// JobConfigurer ...
type JobConfigurer struct {
	presubmits config.Presubmits
	periodics  []prowconfig.Periodic

	ciopConfigs config.CompoundCiopConfig
	templates   []config.ConfigMapSource
	profiles    []config.ConfigMapSource

	prNumber     int
	loggers      Loggers
	allowVolumes bool
	templateMap  map[string]string
}

// NewJobConfigurer ...
func NewJobConfigurer(presubmits config.Presubmits, periodics []prowconfig.Periodic, ciopConfigs config.CompoundCiopConfig, prNumber int, loggers Loggers, allowVolumes bool, templates []config.ConfigMapSource, profiles []config.ConfigMapSource) *JobConfigurer {
	presubmitsFiltered, periodicsFiltered := filterJobs(presubmits, periodics, allowVolumes, loggers.Job)
	return &JobConfigurer{
		presubmits:   presubmitsFiltered,
		periodics:    periodicsFiltered,
		ciopConfigs:  ciopConfigs,
		templates:    templates,
		profiles:     profiles,
		prNumber:     prNumber,
		loggers:      loggers,
		allowVolumes: allowVolumes,
		templateMap:  make(map[string]string, len(templates)),
	}
}

// ConfigureRehearsalJobs filters the jobs that should be rehearsed, then return a list of them re-configured with the
// ci-operator's configuration inlined.
func (jc *JobConfigurer) ConfigureRehearsalJobs() ([]*prowconfig.Presubmit, []prowconfig.Periodic) {
	if jc.allowVolumes {
		for _, t := range jc.templates {
			jc.templateMap[filepath.Base(t.Filename)] = t.TempCMName("template")
		}
	}
	return jc.configurePresubmits(), jc.configurePeriodics()
}

func (jc *JobConfigurer) configurePresubmits() []*prowconfig.Presubmit {
	var rehearsals []*prowconfig.Presubmit
	for repo, jobs := range jc.presubmits {
		for _, job := range jobs {
			jobLogger := jc.loggers.Job.WithFields(logrus.Fields{"target-repo": repo, "target-job": job.Name})
			rehearsal, err := makeRehearsalPresubmit(&job, repo, jc.prNumber)
			if err != nil {
				jobLogger.WithError(err).Warn("Failed to make a rehearsal presubmit")
				continue
			}

			if err := jc.configureJob(rehearsal.Spec, job.Name); err != nil {
				jobLogger.WithError(err).Warn("Failed to inline ci-operator-config into rehearsal presubmit job")
				continue
			}

			jobLogger.WithField(logRehearsalJob, rehearsal.Name).Info("Created a rehearsal job to be submitted")
			rehearsals = append(rehearsals, rehearsal)
		}
	}
	return rehearsals
}

func (jc *JobConfigurer) configurePeriodics() []prowconfig.Periodic {
	var rehearsals []prowconfig.Periodic

	for _, job := range jc.periodics {
		jobLogger := jc.loggers.Job.WithField("target-job", job.Name)
		rehearsal, err := makeRehearsalPeriodic(&job, jc.prNumber)
		if err != nil {
			jobLogger.WithError(err).Warn("Failed to make a rehearsal periodic")
			continue
		}

		if err := jc.configureJob(rehearsal.Spec, job.Name); err != nil {
			jobLogger.WithError(err).Warn("Failed to inline ci-operator-config into rehearsal periodic job")
			continue
		}

		jobLogger.WithField(logRehearsalJob, rehearsal.Name).Info("Created a rehearsal job to be submitted")
		rehearsals = append(rehearsals, rehearsal)
	}

	return rehearsals
}

func (jc *JobConfigurer) configureJob(spec *v1.PodSpec, jobName string) error {
	if err := inlineCiOpConfig(spec.Containers[0], jc.ciopConfigs, jc.loggers); err != nil {
		return err
	}

	if jc.allowVolumes {
		replaceCMTemplateName(spec.Containers[0].VolumeMounts, spec.Volumes, jc.templateMap)
		replaceClusterProfiles(spec.Volumes, jc.profiles, jc.loggers.Debug.WithField("name", jobName))
	}
	return nil
}

// AddRandomJobsForChangedTemplates finds jobs from the PR config that are using a specific template with a specific cluster type.
// The job selection is done by iterating in an unspecified order, which avoids picking the same job
// So if a template will be changed, find the jobs that are using a template in combination with the `aws`,`openstack`,`gcs` and `libvirt` cluster types.
func AddRandomJobsForChangedTemplates(templates []config.ConfigMapSource, toBeRehearsed config.Presubmits, prConfigPresubmits map[string][]prowconfig.Presubmit, loggers Loggers, prNumber int) config.Presubmits {
	rehearsals := make(config.Presubmits)

	for _, template := range templates {
		templateFile := filepath.Base(template.Filename)
		for _, clusterType := range []string{"aws", "gcs", "openstack", "libvirt", "vsphere", "gcp"} {

			if isAlreadyRehearsed(toBeRehearsed, clusterType, templateFile) {
				continue
			}

			if repo, job := pickTemplateJob(prConfigPresubmits, templateFile, clusterType); job != nil {
				jobLogger := loggers.Job.WithFields(logrus.Fields{"target-repo": repo, "target-job": job.Name})
				jobLogger.Info("Picking job to rehearse the template changes")
				rehearsals[repo] = append(rehearsals[repo], *job)
			}
		}
	}
	return rehearsals
}

func isAlreadyRehearsed(toBeRehearsed config.Presubmits, clusterType, templateFile string) bool {
	for _, jobs := range toBeRehearsed {
		for _, job := range jobs {
			if hasClusterType(job, clusterType) && hasTemplateFile(job, templateFile) {
				return true
			}
		}
	}
	return false
}

func replaceCMTemplateName(volumeMounts []v1.VolumeMount, volumes []v1.Volume, mapping map[string]string) {
	for _, volume := range volumes {
		for _, volumeMount := range volumeMounts {
			if name, ok := mapping[volumeMount.SubPath]; ok && volumeMount.Name == volume.Name {
				volume.VolumeSource.ConfigMap.Name = name
			}
		}
	}
}

func pickTemplateJob(presubmits map[string][]prowconfig.Presubmit, templateFile, clusterType string) (string, *prowconfig.Presubmit) {
	var keys []string
	for k := range presubmits {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, repo := range keys {
		for _, job := range presubmits[repo] {
			if job.Agent != string(pjapi.KubernetesAgent) {
				continue
			}

			if hasClusterType(job, clusterType) && hasTemplateFile(job, templateFile) {
				return repo, &job
			}
		}
	}
	return "", nil
}

func hasClusterType(job prowconfig.Presubmit, clusterType string) bool {
	for _, env := range job.Spec.Containers[0].Env {
		if env.Name == clusterTypeEnvName && env.Value == clusterType {
			return true
		}
	}
	return false
}

func hasTemplateFile(job prowconfig.Presubmit, templateFile string) bool {
	if job.Spec.Containers[0].VolumeMounts != nil {
		for _, volumeMount := range job.Spec.Containers[0].VolumeMounts {
			if volumeMount.SubPath == templateFile {
				return true
			}
		}
	}
	return false
}

func replaceClusterProfiles(volumes []v1.Volume, profiles []config.ConfigMapSource, logger *logrus.Entry) {
	nameMap := make(map[string]string, len(profiles))
	for _, p := range profiles {
		nameMap[p.CMName(config.ClusterProfilePrefix)] = p.TempCMName("cluster-profile")
	}
	replace := func(s *v1.VolumeProjection) {
		if s.ConfigMap == nil {
			return
		}
		tmp, ok := nameMap[s.ConfigMap.Name]
		if !ok {
			return
		}
		fields := logrus.Fields{"profile": s.ConfigMap.Name, "tmp": tmp}
		logger.WithFields(fields).Debug("Rehearsal job uses cluster profile, will be replaced by temporary")
		s.ConfigMap.Name = tmp
	}
	for _, v := range volumes {
		if v.Name != "cluster-profile" || v.Projected == nil {
			continue
		}
		for _, s := range v.Projected.Sources {
			replace(&s)
		}
	}
}

// Executor holds all the information needed for the jobs to be executed.
type Executor struct {
	Metrics *ExecutionMetrics

	dryRun     bool
	presubmits []*prowconfig.Presubmit
	periodics  []prowconfig.Periodic
	prNumber   int
	prRepo     string
	refs       *pjapi.Refs
	loggers    Loggers
	pjclient   pj.ProwJobInterface
}

// NewExecutor creates an executor. It also confgures the rehearsal jobs as a list of presubmits.
func NewExecutor(presubmits []*prowconfig.Presubmit, periodics []prowconfig.Periodic, prNumber int, prRepo string, refs *pjapi.Refs,
	dryRun bool, loggers Loggers, pjclient pj.ProwJobInterface) *Executor {
	return &Executor{
		Metrics: &ExecutionMetrics{},

		dryRun:     dryRun,
		presubmits: presubmits,
		periodics:  periodics,
		prNumber:   prNumber,
		prRepo:     prRepo,
		refs:       refs,
		loggers:    loggers,
		pjclient:   pjclient,
	}
}

func printAsYaml(pjs []*pjapi.ProwJob) error {
	sort.Slice(pjs, func(a, b int) bool { return pjs[a].Spec.Job < pjs[b].Spec.Job })
	jobAsYAML, err := yaml.Marshal(pjs)
	if err == nil {
		fmt.Printf("%s\n", jobAsYAML)
	}

	return err
}

// ExecuteJobs takes configs for a set of jobs which should be "rehearsed", and
// creates the ProwJobs that perform the actual rehearsal. *Rehearsal* means
// a "trial" execution of a Prow job configuration when the *job config* config
// is changed, giving feedback to Prow config authors on how the changes of the
// config would affect the "production" Prow jobs run on the actual target repos
func (e *Executor) ExecuteJobs() (bool, error) {
	submitSuccess := true
	pjs, err := e.submitRehearsals()
	if err != nil {
		submitSuccess = false
	}

	if e.dryRun {
		printAsYaml(pjs)

		if submitSuccess {
			return true, nil
		}
		return true, fmt.Errorf("failed to submit all rehearsal jobs")
	}

	req, err := labels.NewRequirement(rehearseLabel, selection.Equals, []string{strconv.Itoa(e.prNumber)})
	if err != nil {
		return false, fmt.Errorf("failed to create label selector: %v", err)
	}
	selector := labels.NewSelector().Add(*req).String()

	names := sets.NewString()
	for _, job := range pjs {
		names.Insert(job.Name)
	}
	waitSuccess, err := e.waitForJobs(names, selector)
	if !submitSuccess {
		return waitSuccess, fmt.Errorf("failed to submit all rehearsal jobs")
	}
	return waitSuccess, err
}

func (e *Executor) waitForJobs(jobs sets.String, selector string) (bool, error) {
	if len(jobs) == 0 {
		return true, nil
	}
	success := true
	for {
		w, err := e.pjclient.Watch(metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return false, fmt.Errorf("failed to create watch for ProwJobs: %v", err)
		}
		defer w.Stop()
		for event := range w.ResultChan() {
			pj, ok := event.Object.(*pjapi.ProwJob)
			if !ok {
				return false, fmt.Errorf("received a %T from watch", event.Object)
			}
			fields := pjutil.ProwJobFields(pj)
			fields["state"] = pj.Status.State
			e.loggers.Debug.WithFields(fields).Debug("Processing ProwJob")
			if !jobs.Has(pj.Name) {
				continue
			}
			switch pj.Status.State {
			case pjapi.FailureState, pjapi.AbortedState, pjapi.ErrorState:
				e.loggers.Job.WithFields(fields).Error("Job failed")
				e.Metrics.FailedRehearsals = append(e.Metrics.FailedRehearsals, pj.Spec.Job)
				success = false
			case pjapi.SuccessState:
				e.loggers.Job.WithFields(fields).Info("Job succeeded")
				e.Metrics.PassedRehearsals = append(e.Metrics.FailedRehearsals, pj.Spec.Job)
			default:
				continue
			}
			jobs.Delete(pj.Name)
			if jobs.Len() == 0 {
				return success, nil
			}
		}
	}
}

func (e *Executor) submitRehearsals() ([]*pjapi.ProwJob, error) {
	var errors []error
	pjs := []*pjapi.ProwJob{}

	for _, job := range e.presubmits {
		created, err := e.submitPresubmit(job)
		if err != nil {
			e.loggers.Job.WithError(err).Warn("Failed to execute a rehearsal presubmit")
			errors = append(errors, err)
			continue
		}
		e.Metrics.SubmittedRehearsals = append(e.Metrics.SubmittedRehearsals, created.Spec.Job)
		e.loggers.Job.WithFields(pjutil.ProwJobFields(created)).Info("Submitted rehearsal prowjob")
		pjs = append(pjs, created)
	}

	for _, job := range e.periodics {
		created, err := e.submitPeriodic(job)
		if err != nil {
			e.loggers.Job.WithError(err).Warn("Failed to execute a rehearsal periodic")
			errors = append(errors, err)
			continue
		}
		e.loggers.Job.WithFields(pjutil.ProwJobFields(created)).Info("Submitted rehearsal prowjob")
		pjs = append(pjs, created)
	}

	return pjs, kerrors.NewAggregate(errors)
}

func (e *Executor) submitPresubmit(job *prowconfig.Presubmit) (*pjapi.ProwJob, error) {
	labels := make(map[string]string)
	for k, v := range job.Labels {
		labels[k] = v
	}

	prowJob := pjutil.NewProwJob(pjutil.PresubmitSpec(*job, *e.refs), labels)
	e.loggers.Job.WithFields(pjutil.ProwJobFields(&prowJob)).Info("Submitting a new prowjob.")

	return e.pjclient.Create(&prowJob)
}

func (e *Executor) submitPeriodic(job prowconfig.Periodic) (*pjapi.ProwJob, error) {
	labels := make(map[string]string)
	for k, v := range job.Labels {
		labels[k] = v
	}

	prowJob := pjutil.NewProwJob(pjutil.PeriodicSpec(job), labels)
	e.loggers.Job.WithFields(pjutil.ProwJobFields(&prowJob)).Info("Submitting a new prowjob.")

	return e.pjclient.Create(&prowJob)
}
