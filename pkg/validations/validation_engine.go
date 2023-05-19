package validations

import (
	// Used to embed yamls by kube-linter
	_ "embed"
	"fmt"
	"os"
	"reflect"
	"strings"

	// Import checks from DVO

	"github.com/app-sre/deployment-validation-operator/pkg/utils"
	_ "github.com/app-sre/deployment-validation-operator/pkg/validations/all"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"golang.stackrox.io/kube-linter/pkg/builtinchecks"
	"golang.stackrox.io/kube-linter/pkg/checkregistry"
	"golang.stackrox.io/kube-linter/pkg/config"
	"golang.stackrox.io/kube-linter/pkg/configresolver"
	"golang.stackrox.io/kube-linter/pkg/diagnostic"
	"golang.stackrox.io/kube-linter/pkg/lintcontext"
	"golang.stackrox.io/kube-linter/pkg/run"

	// Import and initialize all check templates from kube-linter
	_ "golang.stackrox.io/kube-linter/pkg/templates/all"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/spf13/viper"
)

type ValidationEngine interface {
	DeleteMetrics(l prometheus.Labels)
	CheckRegistry() checkregistry.CheckRegistry
	EnabledChecks() []string
	SetEnabledChecks(checks []string)
	RunValidationsForObjects(objects []client.Object, namespaceUID string) (ValidationOutcome, error)
}

type validationEngine struct {
	config           config.Config
	registry         checkregistry.CheckRegistry
	enabledChecks    []string
	registeredChecks map[string]config.Check
	metrics          map[string]*prometheus.GaugeVec
	//cmWatcher        controller.ConfigMapWatcher
}

func NewValidationEngine(configPath string, reg PrometheusRegistry) (ValidationEngine, error) {
	engine := &validationEngine{
		//	cmWatcher: cmWatcher,
	}

	err := engine.LoadConfig(configPath)
	if err != nil {
		return nil, err
	}

	err = engine.InitRegistry(reg)
	if err != nil {
		return nil, err
	}
	return engine, nil
}

func (ve *validationEngine) CheckRegistry() checkregistry.CheckRegistry {
	return ve.registry
}

func (ve *validationEngine) EnabledChecks() []string {
	// better to have lock
	return ve.enabledChecks
}

func (ve *validationEngine) SetEnabledChecks(checks []string) {
	// better to have lock
	ve.enabledChecks = checks
}

// Get info on config file if it exists
func fileExists(filename string) bool {
	info, err := os.Stat(filename)
	if err != nil {
		return false
	}
	return !info.IsDir()
}

func (ve *validationEngine) LoadConfig(path string) error {
	v := viper.New()

	if !fileExists(path) {
		log.Info(fmt.Sprintf("config file %s does not exist. Use default configuration", path))
		path = ""
	}

	// Load Configuration
	config, err := config.Load(v, path)
	if err != nil {
		log.Error(err, "failed to load config")
		return err
	}
	ve.config = config

	return nil
}

type PrometheusRegistry interface {
	Register(prometheus.Collector) error
}

func (ve *validationEngine) InitRegistry(promReg PrometheusRegistry) error {
	disableIncompatibleChecks(&ve.config)
	disableChecks(&ve.config)

	registry := checkregistry.New()
	if err := builtinchecks.LoadInto(registry); err != nil {
		log.Error(err, "failed to load built-in validations")
		return err
	}

	if err := configresolver.LoadCustomChecksInto(&ve.config, registry); err != nil {
		log.Error(err, "failed to load custom checks")
		return err
	}

	enabledChecks, err := configresolver.GetEnabledChecksAndValidate(&ve.config, registry)
	if err != nil {
		log.Error(err, "error finding enabled validations")
		return err
	}

	validationMetrics := map[string]*prometheus.GaugeVec{}
	registeredChecks := map[string]config.Check{}
	for _, checkName := range enabledChecks {
		check := registry.Load(checkName)
		if check == nil {
			return fmt.Errorf("unable to create metric for check %s", checkName)
		}
		registeredChecks[check.Spec.Name] = check.Spec
		metric := newGaugeVecMetric(
			strings.ReplaceAll(check.Spec.Name, "-", "_"),
			fmt.Sprintf("Description: %s ; Remediation: %s",
				check.Spec.Description, check.Spec.Remediation),
			[]string{"namespace_uid", "namespace", "uid", "name", "kind"},
			prometheus.Labels{
				"check_description": check.Spec.Description,
				"check_remediation": check.Spec.Remediation,
			},
		)

		if err := promReg.Register(metric); err != nil {
			return fmt.Errorf("registering metric for check %q: %w", check.Spec.Name, err)
		}

		validationMetrics[checkName] = metric
	}

	ve.registry = registry
	ve.enabledChecks = enabledChecks
	ve.metrics = validationMetrics
	ve.registeredChecks = registeredChecks

	return nil
}

func (ve *validationEngine) GetMetric(name string) *prometheus.GaugeVec {
	m, ok := ve.metrics[name]
	if !ok {
		return nil
	}
	return m
}

func (ve *validationEngine) DeleteMetrics(labels prometheus.Labels) {
	for _, vector := range ve.metrics {
		vector.Delete(labels)
	}
}

func (ve *validationEngine) ClearMetrics(reports []diagnostic.WithContext, labels prometheus.Labels) {
	// Create a list of validation names for use to delete the labels from any
	// metric which isn't in the report but for which there is a metric
	reportValidationNames := map[string]struct{}{}
	for _, report := range reports {
		reportValidationNames[report.Check] = struct{}{}
	}

	// Delete the labels for validations that aren't in the list of reports
	for metricValidationName := range ve.metrics {
		if _, ok := reportValidationNames[metricValidationName]; !ok {
			ve.metrics[metricValidationName].Delete(labels)
		}
	}
}

// InitializeValidationEngine will initialize the validation engine from scratch.
// If an existing engine exists, it will not be replaced with the new one unless all
// initialization steps succeed.
/* func InitializeValidationEngine(configPath string, reg PrometheusRegistry) error {
	ve := validationEngine{}

	err := ve.LoadConfig(configPath)
	if err == nil {
		err = ve.InitRegistry(reg)
	}

	// Only replace the exisiting engine if no errors occurred
	if err == nil {
		engine = ve
	}

	return err
}
*/
func (ve *validationEngine) GetCheckByName(name string) (config.Check, error) {
	check, ok := ve.registeredChecks[name]
	if !ok {
		return config.Check{}, fmt.Errorf("check '%s' is not registered", name)
	}
	return check, nil
}

// disableIncompatibleChecks will forcibly update a kube-linter config
// to disable checks that are incompatible with DVO.
// the same check name may end up in the exclude list multiple times as a result of this; this is OK.
func disableIncompatibleChecks(c *config.Config) {
	c.Checks.Exclude = append(c.Checks.Exclude, getIncompatibleChecks()...)
}

// getIncompatibleChecks returns an array of kube-linter check names that are incompatible with DVO
// these checks involve kube-linter comparing properties from multiple kubernetes objects at once.
// (e.g. "non-existent-service-account" checks that all serviceaccounts referenced by deployment objects
// exist as serviceaccount objects).
// DVO currently only performs a check against a single kubernetes object at a time,
// so these checks that compare multiple objects together will always fail.
func getIncompatibleChecks() []string {
	return []string{
		"dangling-service",
		"non-existent-service-account",
		"non-isolated-pod",
	}
}

// disableChecks will forcibly update a kube-linter config
// to disable checks that do not have supporting openshift documentation
func disableChecks(c *config.Config) {
	c.Checks.Exclude = append(c.Checks.Exclude, getDisabledChecks()...)
}

// getDisabledChecks returns an array of kube-linter check names that are disabled for DVO
// These checks are disabled as they do not have supporting Openshift documentation
// 38 checks... 47 checks according to kube-linter website
func getDisabledChecks() []string {
	return []string{
		"access-to-create-pods",
		"access-to-secrets",
		"cluster-admin-role-binding",
		"default-service-account",
		"deprecated-service-account-field",
		"docker-sock",
		"drop-net-raw-capability",
		"env-var-secret",
		"exposed-services",
		// "host-ipc",
		// "host-network",
		// "host-pid",
		"latest-tag",
		// "minimum-three-replicas",
		"mismatching-selector",
		// "no-anti-affinity",
		"no-extensions-v1beta",
		"no-liveness-probe",
		"no-read-only-root-fs",
		"no-readiness-probe",
		"no-rolling-update-strategy",
		// "privilege-escalation-container",
		// "privileged-container",
		"privileged-ports",
		"read-secret-from-env-var",
		"required-annotation-email",
		"required-label-owner",
		// "run-as-non-root",
		"sensitive-host-mounts",
		"ssh-port",
		"unsafe-proc-mount",
		// "unsafe-sysctls",
		// "unset-cpu-requirements",
		// "unset-memory-requirements",
		"use-namespace",
		"wildcard-in-rules",
		"writable-host-mount",
	}
}

// RunValidationsForObjects runs validation for the group of related objects
func (v *validationEngine) RunValidationsForObjects(objects []client.Object, namespaceUID string) (ValidationOutcome, error) {
	lintCtx := &lintContextImpl{}
	for _, obj := range objects {
		// Only run checks against an object with no owners.  This should be
		// the object that controls the configuration
		if !utils.IsOwner(obj) {
			continue
		}
		// If controller has no replicas clear do not run any validations
		if v.isControllersWithNoReplicas(obj) {
			continue
		}
		lintCtx.addObjects(lintcontext.Object{K8sObject: obj})
	}
	lintCtxs := []lintcontext.LintContext{lintCtx}
	if len(lintCtxs) == 0 {
		return ObjectValidationIgnored, nil
	}
	result, err := run.Run(lintCtxs, v.CheckRegistry(), v.EnabledChecks())
	if err != nil {
		log.Error(err, "error running validations")
		return "", fmt.Errorf("error running validations: %v", err)
	}

	// Clear labels from past run to ensure only results from this run
	// are reflected in the metrics
	for _, o := range objects {
		req := NewRequestFromObject(o)
		req.NamespaceUID = namespaceUID
		v.ClearMetrics(result.Reports, req.ToPromLabels())
	}
	return v.processResult(result, namespaceUID)
}

// isControllersWithNoReplicas checks if the provided object has no replicas
func (v *validationEngine) isControllersWithNoReplicas(obj client.Object) bool {
	objValue := reflect.Indirect(reflect.ValueOf(obj))
	spec := objValue.FieldByName("Spec")
	if spec.IsValid() {
		replicas := spec.FieldByName("Replicas")
		if replicas.IsValid() {
			numReplicas, ok := replicas.Interface().(*int32)

			// clear labels if we fail to get a value for numReplicas, or if value is <= 0
			if !ok || numReplicas == nil || *numReplicas <= 0 {
				req := NewRequestFromObject(obj)
				v.DeleteMetrics(req.ToPromLabels())
				return true
			}
		}
	}
	return false
}

func (v *validationEngine) processResult(result run.Result, namespaceUID string) (ValidationOutcome, error) {
	outcome := ObjectValid
	for _, report := range result.Reports {
		check, err := v.GetCheckByName(report.Check)
		if err != nil {
			log.Error(err, fmt.Sprintf("Failed to get check '%s' by name", report.Check))
			return "", fmt.Errorf("error running validations: %v", err)
		}
		obj := report.Object.K8sObject
		logger := log.WithValues(
			"request.namespace", obj.GetNamespace(),
			"request.name", obj.GetName(),
			"kind", obj.GetObjectKind().GroupVersionKind().Kind,
			"validation", report.Check,
			"check_description", check.Description,
			"check_remediation", report.Remediation,
			"check_failure_reason", report.Diagnostic.Message,
		)
		metric := v.GetMetric(report.Check)
		if metric == nil {
			log.Error(nil, "no metric found for validation", report.Check)
		} else {
			req := NewRequestFromObject(obj)
			req.NamespaceUID = namespaceUID
			metric.With(req.ToPromLabels()).Set(1)
			logger.Info(report.Remediation)
			outcome = ObjectNeedsImprovement
		}
	}
	return outcome, nil
}
