/*
Copyright 2022 SUSE.

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

package controllers

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/utils/pointer"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	kubeyaml "sigs.k8s.io/yaml"

	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/annotations"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"

	bootstrapv1 "github.com/rancher-sandbox/cluster-api-provider-rke2/bootstrap/api/v1alpha1"
	"github.com/rancher-sandbox/cluster-api-provider-rke2/bootstrap/internal/cloudinit"
	controlplanev1 "github.com/rancher-sandbox/cluster-api-provider-rke2/controlplane/api/v1alpha1"
	"github.com/rancher-sandbox/cluster-api-provider-rke2/pkg/locking"
	"github.com/rancher-sandbox/cluster-api-provider-rke2/pkg/rke2"
	"github.com/rancher-sandbox/cluster-api-provider-rke2/pkg/secret"
	bsutil "github.com/rancher-sandbox/cluster-api-provider-rke2/pkg/util"
)

const (
	fileOwner        string = "root:root"
	filePermissions  string = "0640"
	registrationPort int    = 9345
	serverURLFormat  string = "https://%v:%v"
)

// RKE2ConfigReconciler reconciles a Rke2Config object
type RKE2ConfigReconciler struct {
	RKE2InitLock RKE2InitLock
	client.Client
	Scheme *runtime.Scheme
}

const (
	DefaultManifestDirectory string = "/var/lib/rancher/rke2/server/manifests"
)

//+kubebuilder:rbac:groups=bootstrap.cluster.x-k8s.io,resources=rke2configs;rke2configs/status;rke2configs/finalizers,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=controlplane.cluster.x-k8s.io,resources=rke2controlplanes;rke2controlplanes/status,verbs=get;list;watch
//+kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters;clusters/status;machinesets;machines;machines/status;machinepools;machinepools/status,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=secrets;events;configmaps,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Rke2Config object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.12.2/pkg/reconcile
func (r *RKE2ConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (res ctrl.Result, rerr error) {
	logger := log.FromContext(ctx)
	logger.Info("Reconcile RKE2Config")

	config := &bootstrapv1.RKE2Config{}

	if err := r.Get(ctx, req.NamespacedName, config, &client.GetOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Error(err, "rke2Config not found", "rke2-config-name", req.NamespacedName)
			return ctrl.Result{}, err
		}
		logger.Error(err, "", "rke2-config-namespaced-name", req.NamespacedName)
		return ctrl.Result{Requeue: true}, err
	}

	scope := &Scope{}

	machine, err := util.GetOwnerMachine(ctx, r.Client, config.ObjectMeta)
	if err != nil {
		logger.Error(err, "Failed to retrieve owner Machine from the API Server", config.Namespace+"/"+config.Name, "machine", machine.Name)
		return ctrl.Result{}, err
	}
	if machine == nil {
		logger.Info("Machine Controller has not yet set OwnerRef")
		return ctrl.Result{Requeue: true}, nil
	}
	scope.Machine = machine
	logger = logger.WithValues(machine.Kind, machine.GetNamespace()+"/"+machine.GetName(), "resourceVersion", machine.GetResourceVersion())

	// Getting the ControlPlane owner
	cp, err := bsutil.GetOwnerControlPlane(ctx, r.Client, scope.Machine.ObjectMeta)
	if err != nil {
		logger.Error(err, "Failed to retrieve owner ControlPlane from the API Server", config.Namespace+"/"+config.Name, "cluster", cp.Name)
		return ctrl.Result{}, err
	}
	if cp == nil {
		logger.V(5).Info("This config is for a worker node")
		scope.HasControlPlaneOwner = false
	} else {
		logger.Info("This config is for a ControlPlane node")
		scope.HasControlPlaneOwner = true
		scope.ControlPlane = cp
		logger = logger.WithValues(cp.Kind, cp.GetNamespace()+"/"+cp.GetName(), "resourceVersion", cp.GetResourceVersion())
	}

	cluster, err := util.GetClusterByName(ctx, r.Client, machine.GetNamespace(), machine.Spec.ClusterName)
	if err != nil {
		if errors.Cause(err) == util.ErrNoCluster {
			logger.Info(fmt.Sprintf("%s does not belong to a cluster yet, waiting until it's part of a cluster", machine.Kind))
			return ctrl.Result{}, nil
		}

		if apierrors.IsNotFound(err) {
			logger.Info("Cluster does not exist yet, waiting until it is created")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Could not get cluster with metadata")
		return ctrl.Result{}, err
	}

	if annotations.IsPaused(cluster, config) {
		logger.Info("Reconciliation is paused for this object")
		return ctrl.Result{}, nil
	}

	scope.Cluster = cluster
	scope.Config = config
	scope.Logger = logger
	ctx = ctrl.LoggerInto(ctx, logger)

	// Initialize the patch helper.
	patchHelper, err := patch.NewHelper(config, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Attempt to Patch the RKE2Config object and status after each reconciliation if no error occurs.
	defer func() {
		// always update the readyCondition; the summary is represented using the "1 of x completed" notation.
		conditions.SetSummary(config,
			conditions.WithConditions(
				bootstrapv1.DataSecretAvailableCondition,
			),
		)
		// Patch ObservedGeneration only if the reconciliation completed successfully
		patchOpts := []patch.Option{}
		if rerr == nil {
			patchOpts = append(patchOpts, patch.WithStatusObservedGeneration{})
		}
		if err := patchHelper.Patch(ctx, config, patchOpts...); err != nil {
			logger.Error(rerr, "Failed to patch config")
			if rerr == nil {
				rerr = err
			}
		}
	}()

	if !cluster.Status.InfrastructureReady {
		logger.Info("Infrastructure machine not yet ready")
		conditions.MarkFalse(config, bootstrapv1.DataSecretAvailableCondition, bootstrapv1.WaitingForClusterInfrastructureReason, clusterv1.ConditionSeverityInfo, "")
		return ctrl.Result{Requeue: true}, nil
	}
	// Migrate plaintext data to secret.
	// if config.Status.BootstrapData != nil && config.Status.DataSecretName == nil {
	// 	return ctrl.Result{}, r.storeBootstrapData(ctx, scope, config.Status.BootstrapData)
	// }
	// Reconcile status for machines that already have a secret reference, but our status isn't up to date.
	// This case solves the pivoting scenario (or a backup restore) which doesn't preserve the status subresource on objects.
	if machine.Spec.Bootstrap.DataSecretName != nil && (!config.Status.Ready || config.Status.DataSecretName == nil) {
		config.Status.Ready = true
		config.Status.DataSecretName = machine.Spec.Bootstrap.DataSecretName
		conditions.MarkTrue(config, bootstrapv1.DataSecretAvailableCondition)
		return ctrl.Result{}, nil
	}
	// Status is ready means a config has been generated.
	if config.Status.Ready {
		// In any other case just return as the config is already generated and need not be generated again.
		conditions.MarkTrue(config, bootstrapv1.DataSecretAvailableCondition)
		return ctrl.Result{}, nil
	}

	// Note: can't use IsFalse here because we need to handle the absence of the condition as well as false.
	if !conditions.IsTrue(cluster, clusterv1.ControlPlaneInitializedCondition) {
		return r.handleClusterNotInitialized(ctx, scope)
	}

	// Every other case it's a join scenario

	// Unlock any locks that might have been set during init process
	r.RKE2InitLock.Unlock(ctx, cluster)

	// it's a control plane join
	if scope.HasControlPlaneOwner {
		return r.joinControlplane(ctx, scope)
	}

	// It's a worker join
	// GetTheControlPlane for the worker
	wkControlPlane := controlplanev1.RKE2ControlPlane{}
	err = r.Client.Get(ctx, types.NamespacedName{
		Namespace: scope.Cluster.Spec.ControlPlaneRef.Namespace,
		Name:      scope.Cluster.Spec.ControlPlaneRef.Name,
	}, &wkControlPlane)
	if err != nil {
		scope.Logger.Info("Unable to find control plane object for owning Cluster", "error", err)
		return ctrl.Result{Requeue: true}, nil
	}
	scope.ControlPlane = &wkControlPlane
	return r.joinWorker(ctx, scope)
}

// Scope is a scoped struct used during reconciliation.
type Scope struct {
	Logger               logr.Logger
	Config               *bootstrapv1.RKE2Config
	Machine              *clusterv1.Machine
	Cluster              *clusterv1.Cluster
	HasControlPlaneOwner bool
	ControlPlane         *controlplanev1.RKE2ControlPlane
}

// SetupWithManager sets up the controller with the Manager.
func (r *RKE2ConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {

	if r.RKE2InitLock == nil {
		r.RKE2InitLock = locking.NewControlPlaneInitMutex(mgr.GetClient())
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&bootstrapv1.RKE2Config{}).
		Complete(r)
}

// TODO: Implement these functions

// handleClusterNotInitialized handles the first control plane node
func (r *RKE2ConfigReconciler) handleClusterNotInitialized(ctx context.Context, scope *Scope) (res ctrl.Result, reterr error) {

	if !scope.HasControlPlaneOwner {
		scope.Logger.Info("Requeuing because this machine is not a Control Plane machine")
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
	}

	if !r.RKE2InitLock.Lock(ctx, scope.Cluster, scope.Machine) {
		scope.Logger.Info("A control plane is already being initialized, requeuing until control plane is ready")
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
	}

	defer func() {
		if reterr != nil {
			if !r.RKE2InitLock.Unlock(ctx, scope.Cluster) {
				reterr = kerrors.NewAggregate([]error{reterr, errors.New("failed to unlock the rke2 init lock")})
			}
		}
	}()

	certificates := secret.NewCertificatesForInitialControlPlane()
	if err := certificates.LookupOrGenerate(
		ctx,
		r.Client,
		util.ObjectKey(scope.Cluster),
		*metav1.NewControllerRef(scope.Config, bootstrapv1.GroupVersion.WithKind("RKE2Config")),
	); err != nil {
		conditions.MarkFalse(scope.Config, bootstrapv1.CertificatesAvailableCondition, bootstrapv1.CertificatesGenerationFailedReason, clusterv1.ConditionSeverityWarning, err.Error())
		return ctrl.Result{}, err
	}
	conditions.MarkTrue(scope.Config, bootstrapv1.CertificatesAvailableCondition)

	token, err := r.generateAndStoreToken(ctx, scope)
	if err != nil {
		scope.Logger.Error(err, "unable to generate and store an RKE2 server token")
		return ctrl.Result{}, err
	}
	scope.Logger.Info("RKE2 server token generated and stored in Secret!")

	configStruct, configFiles, err := rke2.GenerateInitControlPlaneConfig(
		rke2.RKE2ServerConfigOpts{
			Cluster:              *scope.Cluster,
			ControlPlaneEndpoint: scope.Cluster.Spec.ControlPlaneEndpoint.Host,
			Token:                token,
			ServerURL:            fmt.Sprintf(serverURLFormat, scope.Cluster.Spec.ControlPlaneEndpoint.Host, registrationPort),
			ServerConfig:         scope.ControlPlane.Spec.ServerConfig,
			AgentConfig:          scope.Config.Spec.AgentConfig,
			Ctx:                  ctx,
			Client:               r.Client,
		})

	if err != nil {
		return ctrl.Result{}, err
	}

	b, err := kubeyaml.Marshal(configStruct)
	if err != nil {

		return ctrl.Result{}, err
	}
	scope.Logger.Info("Server config marshalled successfully")

	initConfigFile := bootstrapv1.File{
		Path:        rke2.DefaultRKE2ConfigLocation,
		Content:     string(b),
		Owner:       fileOwner,
		Permissions: filePermissions,
	}

	files, err := r.generateFileListIncludingRegistries(ctx, scope, configFiles)
	if err != nil {
		return ctrl.Result{}, err
	}

	manifestFiles, err := generateFilesFromManifestConfig(ctx, r.Client, scope.ControlPlane.Spec.ManifestsConfigMapReference)
	if err != nil {
		if apierrors.IsNotFound(err) {
			scope.Logger.Error(err, "ConfigMap referenced by manifestsConfigMapReference not found!", "namespace", scope.ControlPlane.Spec.ManifestsConfigMapReference.Namespace, "name", scope.ControlPlane.Spec.ManifestsConfigMapReference.Name)
			return ctrl.Result{}, err
		}
		scope.Logger.Error(err, "Problem when getting ConfigMap referenced by manifestsConfigMapReference", "namespace", scope.ControlPlane.Spec.ManifestsConfigMapReference.Namespace, "name", scope.ControlPlane.Spec.ManifestsConfigMapReference.Name)
		return ctrl.Result{}, err
	}

	files = append(files, manifestFiles...)

	var ntpServers []string
	if scope.Config.Spec.AgentConfig.NTP != nil {
		ntpServers = scope.Config.Spec.AgentConfig.NTP.Servers
	}

	cpinput := &cloudinit.ControlPlaneInput{
		BaseUserData: cloudinit.BaseUserData{
			AirGapped:        scope.Config.Spec.AgentConfig.AirGapped,
			PreRKE2Commands:  scope.Config.Spec.PreRKE2Commands,
			PostRKE2Commands: scope.Config.Spec.PostRKE2Commands,
			ConfigFile:       initConfigFile,
			RKE2Version:      scope.Config.Spec.AgentConfig.Version,
			WriteFiles:       files,
			NTPServers:       ntpServers,
		},
		Certificates: certificates,
	}

	cloudInitData, err := cloudinit.NewInitControlPlane(cpinput)
	if err != nil {
		return ctrl.Result{}, err
	}

	if err := r.storeBootstrapData(ctx, scope, cloudInitData); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// generateFileListIncludingRegistries generates a list of files to be written to disk on the node
// This list includes a registries.yaml file if the user has provided a PrivateRegistriesConfig
// and the files fields provided in the RKE2Config
func (r *RKE2ConfigReconciler) generateFileListIncludingRegistries(ctx context.Context, scope *Scope, configFiles []bootstrapv1.File) ([]bootstrapv1.File, error) {
	registries, registryFiles, err := rke2.GenerateRegistries(rke2.RKE2ConfigRegistry{
		Registry: scope.Config.Spec.PrivateRegistriesConfig,
		Client:   r.Client,
		Ctx:      ctx,
		Logger:   scope.Logger,
	})

	if err != nil {
		scope.Logger.Error(err, "unable to generate registries.yaml for Init Control Plane node")
		return nil, err
	}

	registriesYAML, err := kubeyaml.Marshal(registries)
	if err != nil {
		scope.Logger.Error(err, "unable to marshall registries.yaml")
		return nil, err
	}
	scope.Logger.V(4).Info("Registries.yaml marshalled successfully")

	initRegistriesFile := bootstrapv1.File{
		Path:        rke2.DefaultRKE2RegistriesLocation,
		Content:     string(registriesYAML),
		Owner:       fileOwner,
		Permissions: filePermissions,
	}

	files := append(configFiles, registryFiles...)
	files = append(files, initRegistriesFile)
	files = append(files, scope.Config.Spec.Files...)
	return files, nil
}

type RKE2InitLock interface {
	Unlock(ctx context.Context, cluster *clusterv1.Cluster) bool
	Lock(ctx context.Context, cluster *clusterv1.Cluster, machine *clusterv1.Machine) bool
}

// joinControlPlane implements the part of the Reconciler which bootstraps a secondary
// Control Plane machine joining a cluster that is already initialized
func (r *RKE2ConfigReconciler) joinControlplane(ctx context.Context, scope *Scope) (res ctrl.Result, rerr error) {

	tokenSecret := &corev1.Secret{}
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: scope.Cluster.Namespace, Name: scope.Cluster.Name + "-token"}, tokenSecret); err != nil {
		scope.Logger.Error(err, "Token for already initialized RKE2 Cluster not found", "token-namespace", scope.Cluster.Namespace, "token-name", scope.Cluster.Name+"-token")
		return ctrl.Result{}, err
	}
	token := string(tokenSecret.Data["value"])

	scope.Logger.Info("RKE2 server token found in Secret!")

	if len(scope.ControlPlane.Status.AvailableServerIPs) == 0 {
		scope.Logger.V(3).Info("No ControlPlane IP Address found for node registration")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	configStruct, configFiles, err := rke2.GenerateJoinControlPlaneConfig(
		rke2.RKE2ServerConfigOpts{
			Cluster:              *scope.Cluster,
			Token:                token,
			ControlPlaneEndpoint: scope.Cluster.Spec.ControlPlaneEndpoint.Host,
			ServerURL:            fmt.Sprintf(serverURLFormat, scope.ControlPlane.Status.AvailableServerIPs[0], registrationPort),
			ServerConfig:         scope.ControlPlane.Spec.ServerConfig,
			AgentConfig:          scope.Config.Spec.AgentConfig,
			Ctx:                  ctx,
			Client:               r.Client,
		},
	)

	if err != nil {
		scope.Logger.Error(err, "unable to generate config.yaml for a Secondary Control Plane node")
		return ctrl.Result{}, err
	}

	b, err := kubeyaml.Marshal(configStruct)

	scope.Logger.Info("Showing marshalled config.yaml", "config.yaml", string(b))
	if err != nil {

		return ctrl.Result{}, err
	}
	scope.Logger.Info("Joining Server config marshalled successfully")

	initConfigFile := bootstrapv1.File{
		Path:        rke2.DefaultRKE2ConfigLocation,
		Content:     string(b),
		Owner:       fileOwner,
		Permissions: filePermissions,
	}

	files, err := r.generateFileListIncludingRegistries(ctx, scope, configFiles)
	if err != nil {
		return ctrl.Result{}, err
	}

	manifestFiles, err := generateFilesFromManifestConfig(ctx, r.Client, scope.ControlPlane.Spec.ManifestsConfigMapReference)
	if err != nil {
		if apierrors.IsNotFound(err) {
			scope.Logger.Error(err, "ConfigMap referenced by manifestsConfigMapReference not found!", "namespace", scope.ControlPlane.Spec.ManifestsConfigMapReference.Namespace, "name", scope.ControlPlane.Spec.ManifestsConfigMapReference.Name)
			return ctrl.Result{}, err
		}
		scope.Logger.Error(err, "Problem when getting ConfigMap referenced by manifestsConfigMapReference", "namespace", scope.ControlPlane.Spec.ManifestsConfigMapReference.Namespace, "name", scope.ControlPlane.Spec.ManifestsConfigMapReference.Name)
		return ctrl.Result{}, err
	}

	files = append(files, manifestFiles...)

	var ntpServers []string
	if scope.Config.Spec.AgentConfig.NTP != nil {
		ntpServers = scope.Config.Spec.AgentConfig.NTP.Servers
	}

	cpinput := &cloudinit.ControlPlaneInput{
		BaseUserData: cloudinit.BaseUserData{
			AirGapped:        scope.Config.Spec.AgentConfig.AirGapped,
			PreRKE2Commands:  scope.Config.Spec.PreRKE2Commands,
			PostRKE2Commands: scope.Config.Spec.PostRKE2Commands,
			ConfigFile:       initConfigFile,
			RKE2Version:      scope.Config.Spec.AgentConfig.Version,
			WriteFiles:       files,
			NTPServers:       ntpServers,
		},
	}

	cloudInitData, err := cloudinit.NewJoinControlPlane(cpinput)
	if err != nil {
		return ctrl.Result{}, err
	}

	if err := r.storeBootstrapData(ctx, scope, cloudInitData); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// joinWorker implements the part of the Reconciler which bootstraps a worker node
// after the cluster has been initialized
func (r *RKE2ConfigReconciler) joinWorker(ctx context.Context, scope *Scope) (res ctrl.Result, rerr error) {
	tokenSecret := &corev1.Secret{}
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: scope.Cluster.Namespace, Name: scope.Cluster.Name + "-token"}, tokenSecret); err != nil {
		scope.Logger.Info("Token for already initialized RKE2 Cluster not found", "token-namespace", scope.Cluster.Namespace, "token-name", scope.Cluster.Name+"-token")
		return ctrl.Result{}, err
	}
	token := string(tokenSecret.Data["value"])
	scope.Logger.Info("RKE2 server token found in Secret!")

	if len(scope.ControlPlane.Status.AvailableServerIPs) == 0 {
		scope.Logger.V(3).Info("No ControlPlane IP Address found for node registration")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	configStruct, configFiles, err := rke2.GenerateWorkerConfig(
		rke2.RKE2AgentConfigOpts{
			ServerURL:              fmt.Sprintf(serverURLFormat, scope.ControlPlane.Status.AvailableServerIPs[0], registrationPort),
			Token:                  token,
			AgentConfig:            scope.Config.Spec.AgentConfig,
			Ctx:                    ctx,
			Client:                 r.Client,
			CloudProviderName:      scope.ControlPlane.Spec.ServerConfig.CloudProviderName,
			CloudProviderConfigMap: scope.ControlPlane.Spec.ServerConfig.CloudProviderConfigMap,
		})

	if err != nil {
		return ctrl.Result{}, err
	}
	b, err := kubeyaml.Marshal(configStruct)

	scope.Logger.V(5).Info("Showing marshalled config.yaml", "config.yaml", string(b))
	if err != nil {
		return ctrl.Result{}, err
	}
	scope.Logger.Info("Joining Worker config marshalled successfully")

	wkJoinConfigFile := bootstrapv1.File{
		Path:        rke2.DefaultRKE2ConfigLocation,
		Content:     string(b),
		Owner:       fileOwner,
		Permissions: filePermissions,
	}

	files, err := r.generateFileListIncludingRegistries(ctx, scope, configFiles)
	if err != nil {
		return ctrl.Result{}, err
	}

	var ntpServers []string
	if scope.Config.Spec.AgentConfig.NTP != nil {
		ntpServers = scope.Config.Spec.AgentConfig.NTP.Servers
	}

	wkInput :=
		&cloudinit.BaseUserData{
			PreRKE2Commands:  scope.Config.Spec.PreRKE2Commands,
			AirGapped:        scope.Config.Spec.AgentConfig.AirGapped,
			PostRKE2Commands: scope.Config.Spec.PostRKE2Commands,
			ConfigFile:       wkJoinConfigFile,
			RKE2Version:      scope.Config.Spec.AgentConfig.Version,
			WriteFiles:       files,
			NTPServers:       ntpServers,
		}

	cloudInitData, err := cloudinit.NewJoinWorker(wkInput)
	if err != nil {
		return ctrl.Result{}, err
	}

	if err := r.storeBootstrapData(ctx, scope, cloudInitData); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// generateAndStoreToken generates a random token with 16 characters then stores it in a Secret in the API
func (r *RKE2ConfigReconciler) generateAndStoreToken(ctx context.Context, scope *Scope) (string, error) {
	token, err := bsutil.Random(16)
	if err != nil {
		return "", err
	}

	scope.Logger = scope.Logger.WithValues("cluster-name", scope.Cluster.Name)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      bsutil.TokenName(scope.Cluster.Name),
			Namespace: scope.Config.Namespace,
			Labels: map[string]string{
				clusterv1.ClusterLabelName: scope.Cluster.Name,
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: scope.Cluster.APIVersion,
					Kind:       scope.Cluster.Kind,
					Name:       scope.Cluster.Name,
					UID:        scope.Cluster.UID,
					Controller: pointer.BoolPtr(true),
				},
			},
		},
		Data: map[string][]byte{
			"value": []byte(token),
		},
		Type: clusterv1.ClusterSecretType,
	}

	if err := r.createOrUpdateSecretFromObject(*secret, ctx, scope.Logger, "token", *scope.Config); err != nil {
		return "", err
	}
	return token, nil
}

// storeBootstrapData creates a new secret with the data passed in as input,
// sets the reference in the configuration status and ready to true.
func (r *RKE2ConfigReconciler) storeBootstrapData(ctx context.Context, scope *Scope, data []byte) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      scope.Config.Name,
			Namespace: scope.Config.Namespace,
			Labels: map[string]string{
				clusterv1.ClusterLabelName: scope.Cluster.Name,
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: scope.Config.APIVersion,
					Kind:       scope.Config.Kind,
					Name:       scope.Config.Name,
					UID:        scope.Config.UID,
					Controller: pointer.BoolPtr(true),
				},
			},
		},
		Data: map[string][]byte{
			"value": data,
		},
		Type: clusterv1.ClusterSecretType,
	}

	if err := r.createOrUpdateSecretFromObject(*secret, ctx, scope.Logger, "bootstrap data", *scope.Config); err != nil {
		return err
	}
	scope.Config.Status.DataSecretName = pointer.StringPtr(secret.Name)
	scope.Config.Status.Ready = true
	//	conditions.MarkTrue(scope.Config, bootstrapv1.DataSecretAvailableCondition)
	return nil
}

// createOrUpdateSecret tries to create the given secret in the API, if that secret exists it will update it.
func (r *RKE2ConfigReconciler) createOrUpdateSecretFromObject(secret corev1.Secret, ctx context.Context, logger logr.Logger, secretType string, config bootstrapv1.RKE2Config) (reterr error) {
	if err := r.Client.Create(ctx, &secret); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return errors.Wrapf(err, "failed to create %s secret for %s: %s/%s", secretType, config.Kind, config.Name, config.Namespace)
		}
		logger.Info("%s secret for %s %s already exists, updating", secretType, config.Kind, config.Name)
		if err := r.Client.Update(ctx, &secret); err != nil {
			return errors.Wrapf(err, "failed to update %s secret for %s: %s/%s", secretType, config.Kind, config.Namespace, config.Name)
		}
	}
	return
}

func generateFilesFromManifestConfig(ctx context.Context, cl client.Client, manifestConfigMap corev1.ObjectReference) (files []bootstrapv1.File, err error) {
	if (manifestConfigMap == corev1.ObjectReference{}) {
		return []bootstrapv1.File{}, nil
	}

	manifestSec := &corev1.ConfigMap{}

	err = cl.Get(ctx, types.NamespacedName{
		Namespace: manifestConfigMap.Namespace,
		Name:      manifestConfigMap.Name,
	}, manifestSec)

	if err != nil {
		return
	}

	for filename, content := range manifestSec.Data {
		files = append(files, bootstrapv1.File{
			Path:    DefaultManifestDirectory + "/" + filename,
			Content: string(content),
		})
	}
	return
}
