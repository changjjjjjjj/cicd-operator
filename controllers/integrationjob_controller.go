/*
 Copyright 2021 The CI/CD Operator Authors

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

	"github.com/go-logr/logr"
	tektonv1beta1 "github.com/tektoncd/pipeline/pkg/apis/pipeline/v1beta1"
	"github.com/tmax-cloud/cicd-operator/pkg/pipelinemanager"
	"github.com/tmax-cloud/cicd-operator/pkg/scheduler"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cicdv1 "github.com/tmax-cloud/cicd-operator/api/v1"
)

// IntegrationJobReconciler is an interface for integrationJobReconciler
type IntegrationJobReconciler interface {
	SetupWithManager(mgr ctrl.Manager) error
}

// integrationJobReconciler reconciles a IntegrationJob object
type integrationJobReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme

	scheduler scheduler.Scheduler
	pm        pipelinemanager.PipelineManager
}

// NewIntegrationJobReconciler is a constructor of integrationJobReconciler
func NewIntegrationJobReconciler(cli client.Client, scheme *runtime.Scheme, log logr.Logger) *integrationJobReconciler {
	pm := pipelinemanager.NewPipelineManager(cli, scheme)
	return &integrationJobReconciler{
		Client: cli,
		Scheme: scheme,
		Log:    log,

		pm:        pm,
		scheduler: scheduler.New(cli, scheme, pm),
	}
}

// +kubebuilder:rbac:groups=cicd.tmax.io,resources=integrationjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cicd.tmax.io,resources=integrationjobs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=tekton.dev,resources=pipelineruns,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=tekton.dev,resources=pipelineruns/status,verbs=get

// Reconcile reconciles IntegrationJob
func (r *integrationJobReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("integrationjob", req.NamespacedName)

	// Get IntegrationJob
	instance := &cicdv1.IntegrationJob{}
	if err := r.Client.Get(ctx, req.NamespacedName, instance); err != nil {
		if errors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		log.Error(err, "")
		return reconcile.Result{}, err
	}
	original := instance.DeepCopy()

	exit, err := r.handleFinalizer(instance, original)
	if err != nil {
		log.Error(err, "")
		r.patchJobFailed(instance, original, err.Error())
		return ctrl.Result{}, nil
	}
	if exit {
		return ctrl.Result{}, nil
	}

	// Skip if it's ended
	if instance.Status.CompletionTime != nil {
		return ctrl.Result{}, nil
	}

	// Notify state change to scheduler
	defer r.scheduler.Notify(instance)

	// Get parent IntegrationConfig
	config := &cicdv1.IntegrationConfig{}
	if err := r.Client.Get(ctx, types.NamespacedName{Name: instance.Spec.ConfigRef.Name, Namespace: instance.Namespace}, config); err != nil {
		log.Error(err, "")
		r.patchJobFailed(instance, original, err.Error())
		return ctrl.Result{}, nil
	}

	// Get PipelineRun
	pr := &tektonv1beta1.PipelineRun{}
	if err := r.Client.Get(ctx, types.NamespacedName{Name: pipelinemanager.Name(instance), Namespace: instance.Namespace}, pr); err != nil {
		if !errors.IsNotFound(err) {
			log.Error(err, "")
			r.patchJobFailed(instance, original, err.Error())
			return ctrl.Result{}, nil
		}
		pr = nil
	}

	// Set default values for IntegrationJob.status
	instance.Status.SetDefaults()

	// Check PipelineRun's status and update IntegrationJob's status
	if err := r.pm.ReflectStatus(pr, instance, config); err != nil {
		log.Error(err, "")
		r.patchJobFailed(instance, original, err.Error())
		return ctrl.Result{}, nil
	}

	// Update IntegrationJob
	p := client.MergeFrom(original)
	if err := r.Client.Status().Patch(context.Background(), instance, p); err != nil {
		log.Error(err, "")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *integrationJobReconciler) handleFinalizer(instance, original *cicdv1.IntegrationJob) (bool, error) {
	// Check first if finalizer is already set
	found := false
	idx := -1
	for i, f := range instance.Finalizers {
		if f == finalizer {
			found = true
			idx = i
			break
		}
	}
	if !found {
		instance.Finalizers = append(instance.Finalizers, finalizer)
		p := client.MergeFrom(original)
		if err := r.Client.Patch(context.Background(), instance, p); err != nil {
			return false, err
		}
		return true, nil
	}

	// Deletion check-up
	if instance.DeletionTimestamp != nil && idx >= 0 {
		// Notify scheduler
		r.scheduler.Notify(instance)

		// Delete finalizer
		if len(instance.Finalizers) == 1 {
			instance.Finalizers = nil
		} else {
			last := len(instance.Finalizers) - 1
			instance.Finalizers[idx] = instance.Finalizers[last]
			instance.Finalizers[last] = ""
			instance.Finalizers = instance.Finalizers[:last]
		}

		p := client.MergeFrom(original)
		if err := r.Client.Patch(context.Background(), instance, p); err != nil {
			return false, err
		}

		return true, nil
	}

	return false, nil
}

func (r *integrationJobReconciler) patchJobFailed(instance *cicdv1.IntegrationJob, original *cicdv1.IntegrationJob, message string) {
	instance.Status.State = cicdv1.IntegrationJobStateFailed
	instance.Status.Message = message
	p := client.MergeFrom(original)
	if err := r.Client.Status().Patch(context.Background(), instance, p); err != nil {
		r.Log.Error(err, "")
	}
}

// SetupWithManager sets integrationJobReconciler to the manager
func (r *integrationJobReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cicdv1.IntegrationJob{}).
		Owns(&tektonv1beta1.PipelineRun{}).
		Complete(r)
}
