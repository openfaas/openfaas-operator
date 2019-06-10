package controller

import (
	"encoding/json"
	"strings"

	"github.com/google/go-cmp/cmp"
	faasv1 "github.com/openfaas-incubator/openfaas-operator/pkg/apis/openfaas/v1alpha2"
	appsv1beta2 "k8s.io/api/apps/v1beta2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	glog "k8s.io/klog"
)

const (
	annotationFunctionSpec = "com.openfaas.function.spec"
)

// newDeployment creates a new Deployment for a Function resource. It also sets
// the appropriate OwnerReferences on the resource so handleObject can discover
// the Function resource that 'owns' it.
func newDeployment(
	function *faasv1.Function,
	existingSecrets map[string]*corev1.Secret,
	factory FunctionFactory) *appsv1beta2.Deployment {

	envVars := makeEnvVars(function)
	labels := makeLabels(function)
	nodeSelector := makeNodeSelector(function.Spec.Constraints)
	probes, err := factory.MakeProbes(function)
	if err != nil {
		glog.Warningf("Function %s probes parsing failed: %v",
			function.Spec.Name, err)
	}

	resources, err := makeResources(function)
	if err != nil {
		glog.Warningf("Function %s resources parsing failed: %v",
			function.Spec.Name, err)
	}

	annotations := makeAnnotations(function)

	var serviceAccount string

	if function.Spec.Annotations != nil {
		annotations := *function.Spec.Annotations
		if val, ok := annotations["com.openfaas.serviceaccount"]; ok && len(val) > 0 {
			serviceAccount = val
		}
	}

	deploymentSpec := &appsv1beta2.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        function.Spec.Name,
			Annotations: annotations,
			Namespace:   function.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(function, schema.GroupVersionKind{
					Group:   faasv1.SchemeGroupVersion.Group,
					Version: faasv1.SchemeGroupVersion.Version,
					Kind:    faasKind,
				}),
			},
		},
		Spec: appsv1beta2.DeploymentSpec{
			Replicas: function.Spec.Replicas,
			Strategy: appsv1beta2.DeploymentStrategy{
				Type: appsv1beta2.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1beta2.RollingUpdateDeployment{
					MaxUnavailable: &intstr.IntOrString{
						Type:   intstr.Int,
						IntVal: int32(0),
					},
					MaxSurge: &intstr.IntOrString{
						Type:   intstr.Int,
						IntVal: int32(1),
					},
				},
			},
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app":        function.Spec.Name,
					"controller": function.Name,
				},
			},
			RevisionHistoryLimit: int32p(5),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labels,
					Annotations: annotations,
				},
				Spec: corev1.PodSpec{
					NodeSelector: nodeSelector,
					Containers: []corev1.Container{
						{
							Name:  function.Spec.Name,
							Image: function.Spec.Image,
							Ports: []corev1.ContainerPort{
								{ContainerPort: int32(functionPort), Protocol: corev1.ProtocolTCP},
							},
							ImagePullPolicy: corev1.PullPolicy(factory.Factory.Config.ImagePullPolicy),
							Env:             envVars,
							Resources:       *resources,
							LivenessProbe:   probes.Liveness,
							ReadinessProbe:  probes.Readiness,
						},
					},
				},
			},
		},
	}

	if len(serviceAccount) > 0 {
		deploymentSpec.Spec.Template.Spec.ServiceAccountName = serviceAccount
	}

	factory.ConfigureReadOnlyRootFilesystem(function, deploymentSpec)
	factory.ConfigureContainerUserID(deploymentSpec)

	if err := UpdateSecrets(function, deploymentSpec, existingSecrets); err != nil {
		glog.Warningf("Function %s secrets update failed: %v",
			function.Spec.Name, err)
	}

	return deploymentSpec
}

func makeEnvVars(function *faasv1.Function) []corev1.EnvVar {
	envVars := []corev1.EnvVar{}

	if len(function.Spec.Handler) > 0 {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "fprocess",
			Value: function.Spec.Handler,
		})
	}

	if function.Spec.Environment != nil {
		for k, v := range *function.Spec.Environment {
			envVars = append(envVars, corev1.EnvVar{
				Name:  k,
				Value: v,
			})
		}
	}

	return envVars
}

func makeLabels(function *faasv1.Function) map[string]string {
	labels := map[string]string{
		"faas_function": function.Spec.Name,
		"app":           function.Spec.Name,
		"controller":    function.Name,
	}
	if function.Spec.Labels != nil {
		for k, v := range *function.Spec.Labels {
			labels[k] = v
		}
	}

	return labels
}

func makeAnnotations(function *faasv1.Function) map[string]string {
	annotations := make(map[string]string)

	// disable scraping since the watchdog doesn't expose a metrics endpoint
	annotations["prometheus.io.scrape"] = "false"

	// copy function annotations
	if function.Spec.Annotations != nil {
		for k, v := range *function.Spec.Annotations {
			annotations[k] = v
		}
	}

	// save function spec in deployment annotations
	// used to detect changes in function spec
	specJSON, err := json.Marshal(function.Spec)
	if err != nil {
		glog.Errorf("Failed to marshal function spec: %s", err.Error())
		return annotations
	}

	annotations[annotationFunctionSpec] = string(specJSON)
	return annotations
}

func makeNodeSelector(constraints []string) map[string]string {
	selector := make(map[string]string)

	if len(constraints) > 0 {
		for _, constraint := range constraints {
			parts := strings.Split(constraint, "=")

			if len(parts) == 2 {
				selector[parts[0]] = parts[1]
			}
		}
	}

	return selector
}

// deploymentNeedsUpdate determines if the function spec is different from the deployment spec
func deploymentNeedsUpdate(function *faasv1.Function, deployment *appsv1beta2.Deployment) bool {
	prevFnSpecJson := deployment.ObjectMeta.Annotations[annotationFunctionSpec]
	if prevFnSpecJson == "" {
		// is a new deployment or is an old deployment that is missing the annotation
		return true
	}

	prevFnSpec := &faasv1.FunctionSpec{}
	err := json.Unmarshal([]byte(prevFnSpecJson), prevFnSpec)
	if err != nil {
		glog.Errorf("Failed to parse previous function spec: %s", err.Error())
		return true
	}
	prevFn := faasv1.Function{
		Spec: *prevFnSpec,
	}

	if diff := cmp.Diff(prevFn.Spec, function.Spec); diff != "" {
		glog.V(2).Infof("Change detected for %s diff\n%s", function.Name, diff)
		return true
	} else {
		glog.V(3).Infof("No changes detected for %s", function.Name)
	}

	return false
}

func int32p(i int32) *int32 {
	return &i
}
