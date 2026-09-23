/*
Copyright 2025.

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
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	ManagedSpireAnnotation      = "omegahome.net/managed-spire"
	SVIDEntryIDAnnotation       = "omegahome.net/svid-entry-id"
	SpireFinalizer              = "omegahome.net/spire-finalizer" // Finalizer to ensure SPIRE entries are cleaned up
	KubeConfigVersionAnnotation = "omegahome.net/kubeconfig-secret-version"
)

// ServiceAccountReconciler reconciles a ServiceAccount object
type ServiceAccountReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=core,resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=serviceaccounts/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=core,resources=serviceaccounts/finalizers,verbs=update

func (r *ServiceAccountReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("namespace", req.Namespace)
	sa := &corev1.ServiceAccount{}
	if err := r.Get(ctx, req.NamespacedName, sa); err != nil {
		// if the object is not found, return and don't requeue
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// check for annotations
	if value, exists := sa.Annotations[ManagedSpireAnnotation]; exists && value == "true" {
		logger.V(3).Info("ServiceAccount is managed by SPIRE", "name", sa.Name)
	} else {
		logger.V(3).Info("ServiceAccount is not managed by SPIRE, skipping reconciliation", "name", sa.Name)
		return ctrl.Result{}, nil
	}

	// Check for deletion
	if sa.DeletionTimestamp != nil {
		logger.V(3).Info("ServiceAccount is being deleted", "name", sa.Name)
		err := r.DeleteEntry(ctx, sa)
		if err != nil {
			logger.Error(err, "Failed to delete SPIRE entry for ServiceAccount during cleanup", "name", sa.Name)
			return ctrl.Result{RequeueAfter: 15 * time.Second}, err
		}

		if controllerutil.ContainsFinalizer(sa, SpireFinalizer) {
			controllerutil.RemoveFinalizer(sa, SpireFinalizer)
			if err := r.Update(ctx, sa); err != nil {
				logger.Error(err, "Failed to remove finalizer", "name", sa.Name)
				return ctrl.Result{RequeueAfter: 15 * time.Second}, err
			} else {
				logger.Info("Removed finalizer", "name", sa.Name)
			}
		}
		return ctrl.Result{}, nil
	}

	svidEntryID, hasSVID := sa.Annotations[SVIDEntryIDAnnotation]
	needsRegistration := !hasSVID || svidEntryID == ""

	// Only the spire-agent ServiceAccount carries a kubeconfig that can go stale
	// (e.g. when spire-server-kubeconfig is renewed by cert-manager). For every
	// other managed ServiceAccount, a valid SVID entry is sufficient and no
	// further check is needed.
	var kubeConfigVersion string
	if sa.Name == SpireAgentServiceAccount {
		v, err := r.getKubeConfigSecretVersion(ctx)
		if err != nil {
			logger.Error(err, "Failed to check spire-server-kubeconfig Secret", "name", sa.Name)
			return ctrl.Result{RequeueAfter: 15 * time.Second}, err
		}
		kubeConfigVersion = v
		if !needsRegistration && sa.Annotations[KubeConfigVersionAnnotation] != kubeConfigVersion {
			logger.Info("spire-server-kubeconfig Secret has changed, re-registering", "name", sa.Name)
			needsRegistration = true
		}
	}

	if !needsRegistration {
		logger.V(3).Info("ServiceAccount has a valid SVID", "SVIDEntryID", svidEntryID)
		return ctrl.Result{}, nil
	}

	logger.Info("Registering ServiceAccount with SPIRE...", "name", sa.Name)
	entryID, err := r.CreateEntry(ctx, sa)
	if err != nil {
		logger.Error(err, "Failed to create SPIRE entry for ServiceAccount", "name", sa.Name)
		return ctrl.Result{RequeueAfter: 15 * time.Second}, err
	}
	// Update the ServiceAccount with the SVID entry ID
	sa.Annotations[SVIDEntryIDAnnotation] = string(*entryID)
	if sa.Name == SpireAgentServiceAccount {
		sa.Annotations[KubeConfigVersionAnnotation] = kubeConfigVersion
	}
	if err := r.Update(ctx, sa); err != nil {
		logger.Error(err, "Failed to update ServiceAccount with SVID entryID", "name", sa.Name)
		return ctrl.Result{RequeueAfter: 15 * time.Second}, err
	}
	// Add finalizer to ensure cleanup of SPIRE entry when the ServiceAccount is deleted
	if !controllerutil.ContainsFinalizer(sa, SpireFinalizer) {
		controllerutil.AddFinalizer(sa, SpireFinalizer)
		if err := r.Update(ctx, sa); err != nil {
			logger.Error(err, "Failed to add finalizer ", "name", sa.Name)
			return ctrl.Result{RequeueAfter: 15 * time.Second}, err
		}
	}

	return ctrl.Result{}, nil
}

// getKubeConfigSecretVersion returns the resourceVersion of the spire-server-kubeconfig
// Secret in the controller's own namespace, used to detect rotation.
func (r *ServiceAccountReconciler) getKubeConfigSecretVersion(ctx context.Context) (string, error) {
	ns, err := GetOwnNamespace()
	if err != nil {
		return "", err
	}
	secret := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: SpireKubeConfigSecret}, secret); err != nil {
		return "", err
	}
	return secret.ResourceVersion, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ServiceAccountReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.ServiceAccount{}).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.mapKubeConfigSecretToSpireAgent),
			builder.WithPredicates(predicate.NewPredicateFuncs(r.isSpireKubeConfigSecret)),
		).
		Complete(r)
}

// isSpireKubeConfigSecret restricts the Secret watch to just spire-server-kubeconfig
// in the controller's own namespace, so unrelated Secret changes cluster-wide never
// reach the reconciler.
func (r *ServiceAccountReconciler) isSpireKubeConfigSecret(obj client.Object) bool {
	ns, err := GetOwnNamespace()
	if err != nil {
		return false
	}
	return obj.GetNamespace() == ns && obj.GetName() == SpireKubeConfigSecret
}

// mapKubeConfigSecretToSpireAgent enqueues a reconcile of the spire-agent
// ServiceAccount whenever spire-server-kubeconfig changes, so a rotated
// certificate gets re-registered with SPIRE.
func (r *ServiceAccountReconciler) mapKubeConfigSecretToSpireAgent(_ context.Context, obj client.Object) []reconcile.Request {
	return []reconcile.Request{
		{NamespacedName: types.NamespacedName{Name: SpireAgentServiceAccount, Namespace: obj.GetNamespace()}},
	}
}
