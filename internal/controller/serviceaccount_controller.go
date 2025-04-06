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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	ManagedSpireAnnotation = "omegahome.net/managed-spire"
	SVIDEntryIDAnnotation  = "omegahome.net/svid-entry-id"
	SpireFinalizer         = "omegahome.net/spire-finalizer" // Finalizer to ensure SPIRE entries are cleaned up

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
		logger.Info("ServiceAccount is managed by SPIRE", "name", sa.Name)
	} else {
		logger.Info("ServiceAccount is not managed by SPIRE, skipping reconciliation", "name", sa.Name)
		return ctrl.Result{}, nil
	}

	// Check for deletion
	if sa.DeletionTimestamp != nil {
		logger.Info("ServiceAccount is being deleted", "name", sa.Name)
		err := r.DeleteEntry(ctx, sa)
		if err != nil {
			logger.Error(err, "Failed to delete SPIRE entry for ServiceAccount during cleanup", "name", sa.Name)
			return ctrl.Result{RequeueAfter: 15}, err
		}

		if controllerutil.ContainsFinalizer(sa, SpireFinalizer) {
			controllerutil.RemoveFinalizer(sa, SpireFinalizer)
			if err := r.Update(ctx, sa); err != nil {
				logger.Error(err, "Failed to remove finalizer", "name", sa.Name)
				return ctrl.Result{RequeueAfter: 15}, err
			} else {
				logger.Info("Removed finalizer", "name", sa.Name)
			}
		}
		return ctrl.Result{}, nil
	}

	if svidEntryID, exists := sa.Annotations[SVIDEntryIDAnnotation]; exists && svidEntryID != "" {
		logger.Info("ServiceAccount has a valid SVID", "SVIDEntryID", svidEntryID)
		return ctrl.Result{}, nil

	} else {
		logger.Info("ServiceAccount does not have an SVID. registering...", "name", sa.Name)
		entryID, err := r.CreateEntry(ctx, sa)
		if err != nil {
			logger.Error(err, "Failed to create SPIRE entry for ServiceAccount", "name", sa.Name)
			return ctrl.Result{RequeueAfter: 15}, err
		}
		// Update the ServiceAccount with the SVID entry ID
		sa.Annotations[SVIDEntryIDAnnotation] = string(*entryID)
		if err := r.Update(ctx, sa); err != nil {
			logger.Error(err, "Failed to update ServiceAccount with SVID entryID", "name", sa.Name)
			return ctrl.Result{RequeueAfter: 15}, err
		}
		// Add finalizer to ensure cleanup of SPIRE entry when the ServiceAccount is deleted
		if !controllerutil.ContainsFinalizer(sa, SpireFinalizer) {
			controllerutil.AddFinalizer(sa, SpireFinalizer)
			if err := r.Update(ctx, sa); err != nil {
				logger.Error(err, "Failed to add finalizer ", "name", sa.Name)
				return ctrl.Result{RequeueAfter: 15}, err
			}
		}
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ServiceAccountReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.ServiceAccount{}).
		Complete(r)
}
