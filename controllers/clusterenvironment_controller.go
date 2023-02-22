/*
Copyright 2023 The Primaza Authors.

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
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	primazaiov1alpha1 "github.com/primaza/primaza/api/v1alpha1"
	"github.com/primaza/primaza/pkg/primaza/controlplane"
	"github.com/primaza/primaza/pkg/primaza/workercluster"
)

const clusterEnvironmentFinalizer = "clusterenvironment.primaza.io/finalizer"

var errClusterContextSecretNotFound = fmt.Errorf("Cluster Context Secret not found")

// ClusterEnvironmentReconciler reconciles a ClusterEnvironment object
type ClusterEnvironmentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=primaza.io,resources=clusterenvironments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=primaza.io,resources=clusterenvironments/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=primaza.io,resources=clusterenvironments/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the ClusterEnvironment object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.13.0/pkg/reconcile
func (r *ClusterEnvironmentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)

	// fetch the cluster environment
	ce := &primazaiov1alpha1.ClusterEnvironment{}
	if err := r.Client.Get(ctx, req.NamespacedName, ce); err != nil {
		if apierrors.IsNotFound(err) {
			l.Info("error fetching ClusterEnvironment (deleted)", "error", err)
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}

		l.Error(err, "Failed to get ClusterEnvironment")
		return ctrl.Result{}, err
	}

	// check if instance is marked to be deleted
	if ce.GetDeletionTimestamp() != nil {
		if controllerutil.ContainsFinalizer(ce, clusterEnvironmentFinalizer) {
			// run finalizer
			if err := r.finalizeClusterEnvironment(ctx, ce); err != nil {
				return ctrl.Result{}, err
			}

			// Remove finalizer from cluster environment
			controllerutil.RemoveFinalizer(ce, clusterEnvironmentFinalizer)
			if err := r.Update(ctx, ce); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// add finalizer if needed
	if !controllerutil.ContainsFinalizer(ce, clusterEnvironmentFinalizer) {
		controllerutil.AddFinalizer(ce, clusterEnvironmentFinalizer)
		if err := r.Update(ctx, ce); err != nil {
			return ctrl.Result{}, err
		}
	}

	// test connection
	if err := r.testConnection(ctx, ce); err != nil {
		l.Error(err, "error testing connection")
		return ctrl.Result{}, err
	}

	// reconcile namespaces
	l.Info("reconciling namespaces",
		"application namespaces", ce.Spec.ApplicationNamespaces,
		"service namespaces", ce.Spec.ServiceNamespaces)
	if err := r.reconcileNamespaces(ctx, ce); err != nil {
		l.Error(err, "error reconciling namespaces")
		return ctrl.Result{}, err
	}
	l.Info("namespaces reconciled")

	return ctrl.Result{}, nil
}

func (r *ClusterEnvironmentReconciler) reconcileNamespaces(ctx context.Context, ce *primazaiov1alpha1.ClusterEnvironment) error {
	kcfg, err := r.getClusterRESTConfig(ctx, ce)
	if err != nil {
		return err
	}

	s := controlplane.ClusterEnvironmentState{
		Name:                  ce.Name,
		Namespace:             ce.Namespace,
		ClusterConfig:         kcfg,
		ApplicationNamespaces: ce.Spec.ApplicationNamespaces,
		ServiceNamespaces:     ce.Spec.ServiceNamespaces,
	}

	nr, err := controlplane.NewNamespaceReconciler(s)
	if err != nil {
		return err
	}

	return nr.ReconcileNamespaces(ctx)
}

func (r *ClusterEnvironmentReconciler) testConnection(ctx context.Context, ce *primazaiov1alpha1.ClusterEnvironment) error {
	cfg, err := r.getClusterRESTConfig(ctx, ce)
	if err != nil {
		if errors.Is(err, errClusterContextSecretNotFound) {
			c := workercluster.ConnectionStatus{
				State:   primazaiov1alpha1.ClusterEnvironmentStateOffline,
				Reason:  "ClientCreationError",
				Message: fmt.Sprintf("error creating the client: %s", err),
			}
			if err := r.updateClusterEnvironmentStatus(ctx, ce, c); err != nil {
				return err
			}
		}

		return err
	}

	c := workercluster.TestConnection(ctx, cfg)

	if err := r.updateClusterEnvironmentStatus(ctx, ce, c); err != nil {
		return err
	}

	return nil
}

func (r *ClusterEnvironmentReconciler) updateClusterEnvironmentStatus(ctx context.Context, ce *primazaiov1alpha1.ClusterEnvironment, cs workercluster.ConnectionStatus) error {
	l := log.FromContext(ctx)

	l.Info("updating cluster environment status", "clusterenvironment", ce.GetName(), "connection status", cs)
	ce.Status.State = cs.State
	co := metav1.Condition{
		Type:    string(cs.State),
		Reason:  cs.Reason,
		Message: cs.Message,
		Status:  "True",
	}
	meta.SetStatusCondition(&ce.Status.Conditions, co)
	if err := r.Client.Status().Update(ctx, ce); err != nil {
		l.Error(err, "error updating cluster environment status", "connection status", cs)
		return err
	}

	return nil
}

func (r *ClusterEnvironmentReconciler) getClusterRESTConfig(ctx context.Context, ce *primazaiov1alpha1.ClusterEnvironment) (*rest.Config, error) {
	kc, err := r.getKubeconfig(ctx, ce)
	if err != nil {
		return nil, err
	}

	return clientcmd.RESTConfigFromKubeConfig(kc)
}

func (r *ClusterEnvironmentReconciler) getKubeconfig(ctx context.Context, ce *primazaiov1alpha1.ClusterEnvironment) ([]byte, error) {
	sn := ce.Spec.ClusterContextSecret
	k := client.ObjectKey{Namespace: ce.Namespace, Name: sn}
	var s corev1.Secret
	if err := r.Client.Get(ctx, k, &s); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, errors.Join(errClusterContextSecretNotFound, err)
		}
		return nil, err
	}

	return s.Data["kubeconfig"], nil
}

func (r *ClusterEnvironmentReconciler) finalizeClusterEnvironment(ctx context.Context, ce *primazaiov1alpha1.ClusterEnvironment) error {
	kcfg, err := r.getClusterRESTConfig(ctx, ce)
	if err != nil {
		return err
	}

	s := controlplane.ClusterEnvironmentState{
		Name:                  ce.Name,
		Namespace:             ce.Namespace,
		ClusterConfig:         kcfg,
		ApplicationNamespaces: []string{},
		ServiceNamespaces:     []string{},
	}

	nr, err := controlplane.NewNamespaceReconciler(s)
	if err != nil {
		return err
	}

	return nr.ReconcileNamespaces(ctx)
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClusterEnvironmentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&primazaiov1alpha1.ClusterEnvironment{}).
		Complete(r)
}
