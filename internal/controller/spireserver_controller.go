/*
Copyright 2023.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
	apiErrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spirev1 "github.com/glcp/spire-k8s-operator/api/v1"
	"github.com/go-logr/logr"
)

// SpireServerReconciler reconciles a SpireServer object
type SpireServerReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

var (
	serverNodeAttestors []spirev1.NodeAttestor
	serverPort          int
)

//+kubebuilder:rbac:groups=spire.hpe.com,resources=spireservers,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=spire.hpe.com,resources=spireservers/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=spire.hpe.com,resources=spireservers/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the SpireServer object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.14.4/pkg/reconcile
func (r *SpireServerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = log.FromContext(ctx)

	logger := log.Log.WithValues("SpireServer", req.NamespacedName)

	spireserver := &spirev1.SpireServer{}

	// fetching SPIRE Server instance
	if err := r.Get(ctx, req.NamespacedName, spireserver); err != nil {
		if apiErrors.IsNotFound(err) {
			logger.Error(err, "SPIRE server not found.")
			return ctrl.Result{}, err
		}

		logger.Error(err, "Failed to get SPIRE Server instance.")
		return ctrl.Result{}, err
	}

	if err := validateYaml(spireserver); err != nil {
		if errDelete := r.Delete(ctx, spireserver); errDelete != nil {
			return ctrl.Result{}, fmt.Errorf("failed to delete old instance of CRD: %w, original error: %v", errDelete, err)
		}

		return ctrl.Result{}, err
	}

	serverPort = spireserver.Spec.Port

	serviceAccount := r.createServiceAccount(req.Namespace)

	bundle := r.spireBundleDeployment(req.Namespace)

	roles := r.spireRoleDeployment(req.Namespace)

	roleBinding := r.spireRoleBindingDeployment(req.Namespace)

	clusterRoles := r.spireClusterRoleDeployment(req.Namespace)

	clusterRoleBinding := r.spireClusterRoleBindingDeployment(req.Namespace)

	serverConfigMap := r.spireConfigMapDeployment(spireserver, req.Namespace)

	spireStatefulSet := r.spireStatefulSetDeployment(spireserver.Spec.Replicas, req.Namespace)

	spireService := r.spireServiceDeployment(spireserver.Spec.Port, req.Namespace)

	components := map[string]interface{}{
		"serviceAccount":     serviceAccount,
		"bundle":             bundle,
		"role":               roles,
		"clusterRole":        clusterRoles,
		"roleBinging":        roleBinding,
		"clusterRoleBinding": clusterRoleBinding,
		"serverConfigMap":    serverConfigMap,
		"spireStatefulSet":   spireStatefulSet,
		"spireService":       spireService,
	}

	for key, value := range components {
		err := r.Create(ctx, value.(client.Object))
		result, createError := checkIfFailToCreate(err, key, logger)
		if createError != nil {
			err = createError
			return result, err
		}
	}
	return healthCheck(r, ctx, spireserver, spireStatefulSet)
}

func checkIfFailToCreate(err error, name string, logger logr.Logger) (ctrl.Result, error) {
	if err != nil {
		logger.Error(err, "Failed to create", "Name", name)
	}
	return ctrl.Result{}, err
}

func validateYaml(s *spirev1.SpireServer) error {
	invalidTrustDomain := checkTrustDomain(s.Spec.TrustDomain)

	if invalidTrustDomain != nil {
		return errors.New("trust domain is invalid")
	}

	if (strings.Compare("sqlite3", strings.ToLower(s.Spec.DataStore)) == 0) && (s.Spec.Replicas > 1) {
		return errors.New("cannot have more than 1 replica with sqlite3 database")
	}

	serverNodeAttestors = s.Spec.NodeAttestors

	return nil
}

func checkTrustDomain(trustDomain string) error {
	_, err := spiffeid.TrustDomainFromString(trustDomain)
	return err
}

func (r *SpireServerReconciler) spireClusterRoleBindingDeployment(namespace string) *rbacv1.ClusterRoleBinding {
	subject := rbacv1.Subject{
		Kind:      "ServiceAccount",
		Name:      "spire-server",
		Namespace: namespace,
	}

	clusterRoleBinding := &rbacv1.ClusterRoleBinding{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ClusterRoleBinding",
			APIVersion: "rbac.authorization.k8s.io/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "spire-server-trust-role-binding",
		},
		Subjects: []rbacv1.Subject{
			subject,
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     "spire-server-trust-role",
		},
	}
	return clusterRoleBinding
}

func (r *SpireServerReconciler) spireRoleBindingDeployment(namespace string) *rbacv1.RoleBinding {
	subject := rbacv1.Subject{
		Kind:      "ServiceAccount",
		Name:      "spire-server",
		Namespace: namespace,
	}

	roleBinding := &rbacv1.RoleBinding{
		TypeMeta: metav1.TypeMeta{
			Kind:       "RoleBinding",
			APIVersion: "rbac.authorization.k8s.io/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "spire-server-configmap-role-binding",
			Namespace: namespace,
		},
		Subjects: []rbacv1.Subject{
			subject,
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     "spire-server-configmap-role",
		},
	}
	return roleBinding

}

func (r *SpireServerReconciler) spireClusterRoleDeployment(namespace string) *rbacv1.ClusterRole {
	rules := rbacv1.PolicyRule{
		Verbs:     []string{"create"},
		Resources: []string{"tokenreviews"},
		APIGroups: []string{"authentication.k8s.io"},
	}

	clusterRole := &rbacv1.ClusterRole{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ClusterRole",
			APIVersion: "rbac.authorization.k8s.io/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "spire-server-trust-role",
		},
		Rules: []rbacv1.PolicyRule{
			rules,
		},
	}
	return clusterRole
}

func (r *SpireServerReconciler) spireRoleDeployment(namespace string) *rbacv1.Role {
	rules := rbacv1.PolicyRule{
		Verbs:     []string{"patch", "get", "list"},
		Resources: []string{"configmaps"},
		APIGroups: []string{""},
	}

	serverRole := &rbacv1.Role{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Role",
			APIVersion: "rbac.authorization.k8s.io/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "spire-server-configmap-role",
			Namespace: namespace,
		},
		Rules: []rbacv1.PolicyRule{
			rules,
		},
	}
	return serverRole
}

func (r *SpireServerReconciler) spireBundleDeployment(namespace string) *corev1.ConfigMap {
	bundle := &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ConfigMap",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "spire-bundle",
			Namespace: namespace,
		},
	}
	return bundle
}

func (r *SpireServerReconciler) spireStatefulSetDeployment(replicas int, namespace string) *appsv1.StatefulSet {
	// need to pass in the user desired specs like number of replicas, desired Vols to be mounted, probings,etc.. here
	var numReplicas int32 = int32(replicas)
	labelSelector := metav1.LabelSelector{MatchLabels: map[string]string{"app": "spire-server"}}
	volMount1 := corev1.VolumeMount{
		Name:      "spire-config",
		MountPath: "/run/spire/config",
		ReadOnly:  true,
	}
	volMount2 := corev1.VolumeMount{
		Name:      "spire-data",
		MountPath: "/run/spire/data",
		ReadOnly:  false,
	}
	livenessProbe := corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Path: "/live", Port: intstr.IntOrString{IntVal: 8080}}},
		FailureThreshold:    2,
		InitialDelaySeconds: 15,
		PeriodSeconds:       60,
		TimeoutSeconds:      3,
	}
	readinessProbe := corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Path: "/ready", Port: intstr.IntOrString{IntVal: 8080}}},
		InitialDelaySeconds: 5,
		PeriodSeconds:       5,
	}
	podVolume := corev1.Volume{
		Name: "spire-config",
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: "spire-config-map"},
			},
		},
	}
	containerSpec := corev1.Container{
		Name:           "spire-server",
		Image:          "ghcr.io/spiffe/spire-server:1.5.1",
		Args:           []string{"-config", "/run/spire/config/server.conf"},
		Ports:          []corev1.ContainerPort{{ContainerPort: 8081}},
		VolumeMounts:   []corev1.VolumeMount{volMount1, volMount2},
		LivenessProbe:  &livenessProbe,
		ReadinessProbe: &readinessProbe,
	}
	podSpec := corev1.PodSpec{
		ServiceAccountName: "spire-server",
		Containers:         []corev1.Container{containerSpec},
		Volumes:            []corev1.Volume{podVolume},
	}

	volClaimTemplate := corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "spire-data",
			Namespace: namespace,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.ResourceRequirements{
				Requests: map[corev1.ResourceName]resource.Quantity{
					corev1.ResourceStorage: resource.MustParse("1Gi"),
				},
			},
		},
	}
	statefulSetSpec := appsv1.StatefulSetSpec{
		Replicas: &numReplicas,
		Selector: &labelSelector,
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: namespace,
				Labels:    map[string]string{"app": "spire-server"},
			},
			Spec: podSpec,
		},
		VolumeClaimTemplates: []corev1.PersistentVolumeClaim{volClaimTemplate},
	}
	spireStatefulSet := &appsv1.StatefulSet{
		TypeMeta: metav1.TypeMeta{
			Kind:       "StatefulSet",
			APIVersion: "apps/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "spire-server",
			Namespace: namespace,
			Labels:    map[string]string{"app": "spire-server"},
		},
		Spec: statefulSetSpec,
	}
	return spireStatefulSet
}

func (r *SpireServerReconciler) spireServiceDeployment(port int, namespace string) *corev1.Service {
	// need to pass in the user desired specs like port type,ports,selectors here
	serviceSpec := corev1.ServiceSpec{
		Type:     corev1.ServiceType("NodePort"),
		Ports:    []corev1.ServicePort{{Name: "grpc", Port: int32(port), Protocol: corev1.Protocol("TCP")}},
		Selector: map[string]string{"app": "spire-server"},
	}
	spireService := &corev1.Service{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Service",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "spire-service",
			Namespace: namespace,
		},
		Spec: serviceSpec,
	}
	return spireService
}

// CreateServiceAccount creates a service account for the SPIRE server.
func (r *SpireServerReconciler) createServiceAccount(namespace string) *corev1.ServiceAccount {
	serviceAccount := &corev1.ServiceAccount{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ServiceAccount",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "spire-server",
			Namespace: namespace,
		},
	}
	return serviceAccount
}

func (r *SpireServerReconciler) spireConfigMapDeployment(s *spirev1.SpireServer, namespace string) *corev1.ConfigMap {
	nodeAttestorsConfig := ""

	for _, nodeAttestor := range s.Spec.NodeAttestors {
		if strings.Compare(string(nodeAttestor.Name), "join_token") == 0 {
			nodeAttestorsConfig += joinTokenNodeAttestor()
		} else if strings.Compare(string(nodeAttestor.Name), "k8s_sat") == 0 {
			nodeAttestorsConfig += k8sSatNodeAttestor(namespace)
		} else if strings.Compare(string(nodeAttestor.Name), "k8s_psat") == 0 {
			nodeAttestorsConfig += k8sPsatNodeAttestor(namespace)
		}
	}

	config := serverCreation(strconv.Itoa(s.Spec.Port), s.Spec.TrustDomain) +
		plugins(nodeAttestorsConfig, s.Spec.KeyStorage, namespace, s.Spec.DataStore, s.Spec.ConnectionString) +
		healthChecks()

	configMap := &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ConfigMap",
			APIVersion: "v1",
		},

		ObjectMeta: metav1.ObjectMeta{
			Name:      "spire-config-map",
			Namespace: namespace,
		},

		Data: map[string]string{
			"server.conf": config,
		},
	}

	return configMap
}

func k8sSatNodeAttestor(namespace string) string {
	return `

	NodeAttestor "k8s_sat" {
		plugin_data {
			clusters = {
				"demo-cluster" = {
					use_token_review_api_validation = true
					service_account_allow_list = ["` + namespace + `:spire-agent"]
				}
			}
		}
	}`
}

func k8sPsatNodeAttestor(namespace string) string {
	return `

	NodeAttestor "k8s_psat" {
		plugin_data {
			clusters = {
				"cluster" = {
					service_account_allow_list = ["` + namespace + `:spire-agent"]
				}
			}
		}
	}`
}

func joinTokenNodeAttestor() string {
	return `

	NodeAttestor "join_token" {
		plugin_data {

		}
	}`
}

func serverCreation(bindingPort string, trustDomain string) string {
	return `
	server {
		bind_address = "0.0.0.0"
		bind_port = "` + bindingPort + `"
		socket_path = "/tmp/spire-server/private/api.sock"
		trust_domain = "` + trustDomain + `"
		data_dir = "/run/spire/data"
		log_level = "DEBUG"
		ca_key_type = "rsa-2048"
	
		ca_subject = {
			country = ["US"],
			organization = ["SPIFFE"],
			common_name = "",
		}
	}`
}

func plugins(nodeAttestorsConfig string, keyStorage string, namespace string, datastore string, connectionString string) string {
	return `

	plugins {
		DataStore "sql" {
			plugin_data {
			  database_type = "` + datastore + `"
			  connection_string = "` + connectionString + `"
			}
		}` +
		nodeAttestorsConfig + `
	
		KeyManager "` + keyStorage + `" {
			plugin_data {
				keys_path = "/run/spire/data/keys.json"
			}
		}
	
		Notifier "k8sbundle" {
			plugin_data {
				namespace = "` + namespace + `"
			}
		}
	}`
}

func healthChecks() string {
	return `

health_checks {
	listener_enabled = true
	bind_address = "0.0.0.0"
	bind_port = "8080"
	live_path = "/live"
	ready_path = "/ready"
}`
}

func healthCheck(r *SpireServerReconciler, ctx context.Context, s *spirev1.SpireServer,
	statefulSet *appsv1.StatefulSet) (ctrl.Result, error) {
	quit := make(chan bool, 1)

	ticker := time.NewTicker(5 * time.Second)
	var podList corev1.PodList

	for {
		select {
		case <-quit:
			ticker.Stop()
			return ctrl.Result{}, nil

		case <-ticker.C:
			statCount := make(map[string]int)

			if err := r.List(ctx, &podList); err != nil {
				return ctrl.Result{}, err
			}

			for _, pod := range podList.Items {
				valid := false

				if strings.Contains(pod.Name, statefulSet.Name) {
					for _, condition := range pod.Status.Conditions {
						if condition.Status == "True" {
							valid = true
							updateStatusMap(statCount, condition.Type)

							if condition.Type == "Ready" {
								break
							}
						}
					}

					if !valid {
						statCount["err"]++
					}
				}
			}

			replicas := int(*statefulSet.Spec.Replicas)
			err := updateHealth(statCount, s, replicas, ctx, r)

			if err != nil {
				return ctrl.Result{}, err
			}
		}
	}
}

func updateHealth(statCount map[string]int, s *spirev1.SpireServer, replicas int, ctx context.Context, r *SpireServerReconciler) error {
	if statCount["err"] > 0 {
		s.Status.Health = "ERROR"
	} else if statCount["ready"] == replicas {
		s.Status.Health = "READY"
	} else if statCount["live"] == replicas {
		s.Status.Health = "LIVE"
	} else {
		s.Status.Health = "INITIALIZING"
	}

	if err := r.Status().Update(ctx, s); err != nil {
		return err
	}

	return nil
}

func updateStatusMap(statCount map[string]int, podConditionType corev1.PodConditionType) {
	if podConditionType == "Ready" {
		statCount["ready"]++
	} else if podConditionType == "Initialized" {
		statCount["live"]++
	} else if podConditionType == "ContainersReady" || podConditionType == "PodScheduled" {
		statCount["init"]++
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *SpireServerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&spirev1.SpireServer{}).
		Complete(r)
}
