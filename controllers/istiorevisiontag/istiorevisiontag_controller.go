// Copyright Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package istiorevisiontag

import (
	"context"
	"errors"
	"fmt"
	"path"
	"reflect"

	"github.com/go-logr/logr"
	"github.com/istio-ecosystem/sail-operator/api/v1alpha1"
	"github.com/istio-ecosystem/sail-operator/pkg/constants"
	"github.com/istio-ecosystem/sail-operator/pkg/enqueuelogger"
	"github.com/istio-ecosystem/sail-operator/pkg/errlist"
	"github.com/istio-ecosystem/sail-operator/pkg/helm"
	"github.com/istio-ecosystem/sail-operator/pkg/kube"
	"github.com/istio-ecosystem/sail-operator/pkg/reconciler"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"istio.io/istio/pkg/ptr"
)

const (
	IstioInjectionLabel        = "istio-injection"
	IstioInjectionEnabledValue = "enabled"
	IstioRevLabel              = "istio.io/rev"
	IstioSidecarInjectLabel    = "sidecar.istio.io/inject"

	revisionTagsChartName = "revisiontags"

	sailOperatorReferencedRevisionLabel = "sailoperator.io/referenced-revision"
)

// Reconciler reconciles an IstioRevisionTag object
type Reconciler struct {
	client.Client
	Scheme            *runtime.Scheme
	ResourceDirectory string
	ChartManager      *helm.ChartManager
}

func NewReconciler(client client.Client, scheme *runtime.Scheme, resourceDir string, chartManager *helm.ChartManager) *Reconciler {
	return &Reconciler{
		Client:            client,
		Scheme:            scheme,
		ResourceDirectory: resourceDir,
		ChartManager:      chartManager,
	}
}

// +kubebuilder:rbac:groups=sailoperator.io,resources=istiorevisiontags,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=sailoperator.io,resources=istiorevisiontags/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=sailoperator.io,resources=istiorevisiontags/finalizers,verbs=update
// +kubebuilder:rbac:groups="admissionregistration.k8s.io",resources=mutatingwebhookconfigurations,verbs="*"
// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.14.1/pkg/reconcile
func (r *Reconciler) Reconcile(ctx context.Context, tag *v1alpha1.IstioRevisionTag) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	rev, reconcileErr := r.doReconcile(ctx, tag)

	log.Info("Reconciliation done. Updating labels and status.")
	labelsErr := r.updateLabels(ctx, tag, rev)
	statusErr := r.updateStatus(ctx, tag, rev, reconcileErr)

	return ctrl.Result{}, errors.Join(reconcileErr, labelsErr, statusErr)
}

func (r *Reconciler) doReconcile(ctx context.Context, tag *v1alpha1.IstioRevisionTag) (*v1alpha1.IstioRevision, error) {
	log := logf.FromContext(ctx)
	if err := r.validate(ctx, tag); err != nil {
		return nil, err
	}

	rev, err := r.getIstioRevision(ctx, tag.Spec.TargetRef)
	if rev == nil || err != nil {
		return nil, fmt.Errorf("failed to retrieve IstioRevision for IstioRevisionTag %q: %w", tag.Name, err)
	}

	log.Info("Installing Helm chart")
	return rev, r.installHelmCharts(ctx, tag, rev)
}

func (r *Reconciler) Finalize(ctx context.Context, tag *v1alpha1.IstioRevisionTag) error {
	return r.uninstallHelmCharts(ctx, tag)
}

func (r *Reconciler) validate(ctx context.Context, tag *v1alpha1.IstioRevisionTag) error {
	if tag.Spec.TargetRef.Kind == "" || tag.Spec.TargetRef.Name == "" {
		return ReferenceNotFoundError{"spec.targetRef not set"}
	}
	rev := v1alpha1.IstioRevision{}
	if err := r.Client.Get(ctx, types.NamespacedName{Name: tag.Name}, &rev); !apierrors.IsNotFound(err) {
		return NameAlreadyExistsError{Message: "there is an IstioRevision with this name"}
	}
	if tag.Spec.TargetRef.Kind == v1alpha1.IstioKind {
		i := v1alpha1.Istio{}
		if err := r.Client.Get(ctx, types.NamespacedName{Name: tag.Spec.TargetRef.Name}, &i); err != nil {
			if apierrors.IsNotFound(err) {
				return ReferenceNotFoundError{"referenced Istio resource does not exist"}
			}
			return reconciler.NewValidationError("failed to get referenced Istio resource: " + err.Error())
		}
	} else if tag.Spec.TargetRef.Kind == v1alpha1.IstioRevisionKind {
		if err := r.Client.Get(ctx, types.NamespacedName{Name: tag.Spec.TargetRef.Name}, &rev); err != nil {
			if apierrors.IsNotFound(err) {
				return ReferenceNotFoundError{"referenced IstioRevision resource does not exist"}
			}
			return reconciler.NewValidationError("failed to get referenced IstioRevision resource: " + err.Error())
		}
	}
	return nil
}

func (r *Reconciler) getIstioRevision(ctx context.Context, ref v1alpha1.IstioRevisionTagTargetReference) (*v1alpha1.IstioRevision, error) {
	var revisionName string
	if ref.Kind == v1alpha1.IstioRevisionKind {
		revisionName = ref.Name
	} else if ref.Kind == v1alpha1.IstioKind {
		i := v1alpha1.Istio{}
		err := r.Client.Get(ctx, types.NamespacedName{Name: ref.Name}, &i)
		if err != nil {
			// TODO: handle not found
			return nil, err
		}
		if i.Status.ActiveRevisionName == "" {
			return nil, fmt.Errorf("referenced Istio has no active revision")
		}
		revisionName = i.Status.ActiveRevisionName
	} else {
		return nil, fmt.Errorf("unknown targetRef.kind")
	}

	rev := v1alpha1.IstioRevision{}
	err := r.Client.Get(ctx, types.NamespacedName{Name: revisionName}, &rev)
	if err != nil {
		// TODO: handle not found
		return nil, err
	}
	return &rev, nil
}

func (r *Reconciler) installHelmCharts(ctx context.Context, tag *v1alpha1.IstioRevisionTag, rev *v1alpha1.IstioRevision) error {
	ownerReference := metav1.OwnerReference{
		APIVersion:         v1alpha1.GroupVersion.String(),
		Kind:               v1alpha1.IstioRevisionTagKind,
		Name:               tag.Name,
		UID:                tag.UID,
		Controller:         ptr.Of(true),
		BlockOwnerDeletion: ptr.Of(true),
	}

	// TODO: add in values not in spec
	rev.Spec.Values.RevisionTags = []string{tag.Name}
	values := helm.FromValues(rev.Spec.Values)

	_, err := r.ChartManager.UpgradeOrInstallChart(ctx, r.getChartDir(rev),
		values, rev.Spec.Namespace, getReleaseName(tag), ownerReference)
	if err != nil {
		return fmt.Errorf("failed to install/update Helm chart %q: %w", revisionTagsChartName, err)
	}
	return nil
}

func getReleaseName(tag *v1alpha1.IstioRevisionTag) string {
	return fmt.Sprintf("%s-%s", tag.Name, revisionTagsChartName)
}

func (r *Reconciler) getChartDir(tag *v1alpha1.IstioRevision) string {
	return path.Join(r.ResourceDirectory, tag.Spec.Version, "charts", revisionTagsChartName)
}

func (r *Reconciler) uninstallHelmCharts(ctx context.Context, tag *v1alpha1.IstioRevisionTag) error {
	if _, err := r.ChartManager.UninstallChart(ctx, getReleaseName(tag), tag.Status.IstiodNamespace); err != nil {
		return fmt.Errorf("failed to uninstall Helm chart %q: %w", revisionTagsChartName, err)
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	logger := mgr.GetLogger().WithName("ctrlr").WithName("revtag")

	// mainObjectHandler handles the IstioRevisionTag watch events
	mainObjectHandler := wrapEventHandler(logger, &handler.EnqueueRequestForObject{})

	// ownedResourceHandler handles resources that are owned by the IstioRevisionTag CR
	ownedResourceHandler := wrapEventHandler(
		logger, handler.EnqueueRequestForOwner(r.Scheme, r.RESTMapper(), &v1alpha1.IstioRevisionTag{}, handler.OnlyControllerOwner()))

	// operatorResourcesHandler handles watch events from operator CRDs Istio and IstioRevision
	operatorResourcesHandler := wrapEventHandler(logger, handler.EnqueueRequestsFromMapFunc(r.mapOperatorResourceToReconcileRequest))
	// nsHandler triggers reconciliation in two cases:
	// - when a namespace that references the IstioRevisionTag CR via the istio.io/rev
	//   or istio-injection labels is updated, so that the InUse condition of
	//   the IstioRevisionTag CR is updated.
	nsHandler := wrapEventHandler(logger, handler.EnqueueRequestsFromMapFunc(r.mapNamespaceToReconcileRequest))

	// podHandler handles pods that reference the IstioRevisionTag CR via the istio.io/rev or sidecar.istio.io/inject labels.
	// The handler triggers the reconciliation of the referenced IstioRevision CR so that its InUse condition is updated.
	podHandler := wrapEventHandler(logger, handler.EnqueueRequestsFromMapFunc(r.mapPodToReconcileRequest))

	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{
			LogConstructor: func(req *reconcile.Request) logr.Logger {
				log := logger
				if req != nil {
					log = log.WithValues("IstioRevisionTag", req.Name)
				}
				return log
			},
		}).
		// we use the Watches function instead of For(), so that we can wrap the handler so that events that cause the object to be enqueued are logged
		Watches(&v1alpha1.IstioRevisionTag{}, mainObjectHandler).
		Named("istiorevisiontag").
		// watches related to in-use detection
		Watches(&corev1.Namespace{}, nsHandler, builder.WithPredicates(ignoreStatusChange())).
		Watches(&corev1.Pod{}, podHandler, builder.WithPredicates(ignoreStatusChange())).

		// cluster-scoped resources
		Watches(&v1alpha1.Istio{}, operatorResourcesHandler).
		Watches(&v1alpha1.IstioRevision{}, operatorResourcesHandler).
		Watches(&admissionv1.MutatingWebhookConfiguration{}, ownedResourceHandler).
		Complete(reconciler.NewStandardReconcilerWithFinalizer[*v1alpha1.IstioRevisionTag](r.Client, r.Reconcile, r.Finalize, constants.FinalizerName))
}

func (r *Reconciler) determineStatus(ctx context.Context, tag *v1alpha1.IstioRevisionTag,
	rev *v1alpha1.IstioRevision, reconcileErr error,
) (v1alpha1.IstioRevisionTagStatus, error) {
	var errs errlist.Builder
	reconciledCondition := r.determineReconciledCondition(reconcileErr)

	inUseCondition, err := r.determineInUseCondition(ctx, tag)
	errs.Add(err)

	status := *tag.Status.DeepCopy()
	status.ObservedGeneration = tag.Generation
	if reconciledCondition.Status == metav1.ConditionTrue && rev != nil {
		status.IstiodNamespace = rev.Spec.Namespace
		status.IstioRevision = rev.Name
	}
	status.SetCondition(reconciledCondition)
	status.SetCondition(inUseCondition)
	status.State = deriveState(reconciledCondition, inUseCondition)
	return status, errs.Error()
}

func (r *Reconciler) updateStatus(ctx context.Context, tag *v1alpha1.IstioRevisionTag, rev *v1alpha1.IstioRevision, reconcileErr error) error {
	var errs errlist.Builder

	status, err := r.determineStatus(ctx, tag, rev, reconcileErr)
	if err != nil {
		errs.Add(fmt.Errorf("failed to determine status: %w", err))
	}

	if !reflect.DeepEqual(tag.Status, status) {
		if err := r.Client.Status().Patch(ctx, tag, kube.NewStatusPatch(status)); err != nil {
			errs.Add(fmt.Errorf("failed to patch status: %w", err))
		}
	}
	return errs.Error()
}

func deriveState(reconciledCondition, inUseCondition v1alpha1.IstioRevisionTagCondition) v1alpha1.IstioRevisionTagConditionReason {
	if reconciledCondition.Status != metav1.ConditionTrue {
		return reconciledCondition.Reason
	}
	if inUseCondition.Status != metav1.ConditionTrue {
		return inUseCondition.Reason
	}
	return v1alpha1.IstioRevisionTagReasonHealthy
}

func (r *Reconciler) determineReconciledCondition(err error) v1alpha1.IstioRevisionTagCondition {
	c := v1alpha1.IstioRevisionTagCondition{Type: v1alpha1.IstioRevisionTagConditionReconciled}

	if err == nil {
		c.Status = metav1.ConditionTrue
	} else if IsNameAlreadyExistsError(err) {
		c.Status = metav1.ConditionFalse
		c.Reason = v1alpha1.IstioRevisionTagReasonNameAlreadyExists
		c.Message = err.Error()
	} else if IsReferenceNotFoundError(err) {
		c.Status = metav1.ConditionFalse
		c.Reason = v1alpha1.IstioRevisionTagReasonReferenceNotFound
		c.Message = err.Error()
	} else {
		c.Status = metav1.ConditionFalse
		c.Reason = v1alpha1.IstioRevisionTagReasonReconcileError
		c.Message = fmt.Sprintf("error reconciling resource: %v", err)
	}
	return c
}

func (r *Reconciler) determineInUseCondition(ctx context.Context, tag *v1alpha1.IstioRevisionTag) (v1alpha1.IstioRevisionTagCondition, error) {
	c := v1alpha1.IstioRevisionTagCondition{Type: v1alpha1.IstioRevisionTagConditionInUse}

	isReferenced, err := r.isRevisionTagReferencedByWorkloads(ctx, tag)
	if err == nil {
		if isReferenced {
			c.Status = metav1.ConditionTrue
			c.Reason = v1alpha1.IstioRevisionTagReasonReferencedByWorkloads
			c.Message = "Referenced by at least one pod or namespace"
		} else {
			c.Status = metav1.ConditionFalse
			c.Reason = v1alpha1.IstioRevisionTagReasonNotReferenced
			c.Message = "Not referenced by any pod or namespace"
		}
		return c, nil
	}
	c.Status = metav1.ConditionUnknown
	c.Reason = v1alpha1.IstioRevisionTagReasonUsageCheckFailed
	c.Message = fmt.Sprintf("failed to determine if revision tag is in use: %v", err)
	return c, fmt.Errorf("failed to determine if IstioRevisionTag is in use: %w", err)
}

func (r *Reconciler) updateLabels(ctx context.Context, tag *v1alpha1.IstioRevisionTag, rev *v1alpha1.IstioRevision) error {
	updatedTag := tag.DeepCopy()
	if rev == nil {
		delete(updatedTag.Labels, sailOperatorReferencedRevisionLabel)
	} else {
		if updatedTag.Labels == nil {
			updatedTag.Labels = make(map[string]string, 1)
		}
		updatedTag.Labels[sailOperatorReferencedRevisionLabel] = rev.Name
	}
	return r.Patch(ctx, updatedTag, client.MergeFrom(tag))
}

func (r *Reconciler) isRevisionTagReferencedByWorkloads(ctx context.Context, tag *v1alpha1.IstioRevisionTag) (bool, error) {
	log := logf.FromContext(ctx)
	nsList := corev1.NamespaceList{}
	nsMap := map[string]corev1.Namespace{}
	if err := r.Client.List(ctx, &nsList); err != nil { // TODO: can we optimize this by specifying a label selector
		return false, fmt.Errorf("failed to list namespaces: %w", err)
	}
	for _, ns := range nsList.Items {
		if namespaceReferencesRevisionTag(ns, tag) {
			log.V(2).Info("RevisionTag is referenced by Namespace", "Namespace", ns.Name)
			return true, nil
		}
		nsMap[ns.Name] = ns
	}

	podList := corev1.PodList{}
	if err := r.Client.List(ctx, &podList); err != nil { // TODO: can we optimize this by specifying a label selector
		return false, fmt.Errorf("failed to list pods: %w", err)
	}
	for _, pod := range podList.Items {
		if podReferencesRevisionTag(pod, tag) {
			log.V(2).Info("RevisionTag is referenced by Pod", "Pod", client.ObjectKeyFromObject(&pod))
			return true, nil
		}
	}

	rev, err := r.getIstioRevision(ctx, tag.Spec.TargetRef)
	if err != nil {
		return false, err
	}

	if tag.Name == v1alpha1.DefaultRevision && rev.Spec.Values != nil &&
		rev.Spec.Values.SidecarInjectorWebhook != nil &&
		rev.Spec.Values.SidecarInjectorWebhook.EnableNamespacesByDefault != nil &&
		*rev.Spec.Values.SidecarInjectorWebhook.EnableNamespacesByDefault {
		return true, nil
	}

	log.V(2).Info("RevisionTag is not referenced by any Pod or Namespace")
	return false, nil
}

func namespaceReferencesRevisionTag(ns corev1.Namespace, tag *v1alpha1.IstioRevisionTag) bool {
	return tag.Name == getReferencedRevisionFromNamespace(ns.Labels)
}

func podReferencesRevisionTag(pod corev1.Pod, tag *v1alpha1.IstioRevisionTag) bool {
	return tag.Name == getReferencedRevisionTagFromPod(pod.GetLabels())
}

func getReferencedRevisionFromNamespace(labels map[string]string) string {
	if labels[IstioInjectionLabel] == IstioInjectionEnabledValue {
		return v1alpha1.DefaultRevision
	}
	revision := labels[IstioRevLabel]
	if revision != "" {
		return revision
	}
	// TODO: if .Values.sidecarInjectorWebhook.enableNamespacesByDefault is true, then all namespaces except system namespaces should use the "default" revision

	return ""
}

func getReferencedRevisionTagFromPod(podLabels map[string]string) string {
	// we only look at pod labels to identify injection intent, as the annotation only references the real revision name instead of the tag
	if podLabels[IstioInjectionLabel] == IstioInjectionEnabledValue {
		return v1alpha1.DefaultRevision
	} else if podLabels[IstioSidecarInjectLabel] != "false" && podLabels[IstioRevLabel] != "" {
		return podLabels[IstioRevLabel]
	}

	return ""
}

func (r *Reconciler) mapNamespaceToReconcileRequest(ctx context.Context, ns client.Object) []reconcile.Request {
	var requests []reconcile.Request

	// Check if the namespace references an IstioRevisionTag in its labels
	revision := getReferencedRevisionFromNamespace(ns.GetLabels())
	if revision != "" {
		requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: revision}})
	}
	return requests
}

func (r *Reconciler) mapPodToReconcileRequest(ctx context.Context, pod client.Object) []reconcile.Request {
	revision := getReferencedRevisionTagFromPod(pod.GetLabels())
	if revision != "" {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: revision}}}
	}
	return nil
}

func (r *Reconciler) mapOperatorResourceToReconcileRequest(ctx context.Context, obj client.Object) []reconcile.Request {
	var revisionName string
	if i, ok := obj.(*v1alpha1.Istio); ok && i.Status.ActiveRevisionName != "" {
		revisionName = i.Status.ActiveRevisionName
	} else if rev, ok := obj.(*v1alpha1.IstioRevision); ok {
		revisionName = rev.Name
	} else {
		return nil
	}
	tags := v1alpha1.IstioRevisionTagList{}
	labelSelector := map[string]string{
		sailOperatorReferencedRevisionLabel: revisionName,
	}
	err := r.Client.List(ctx, &tags, &client.ListOptions{LabelSelector: labels.SelectorFromSet(labelSelector)})
	if err != nil {
		return nil
	}
	requests := []reconcile.Request{}
	for _, revision := range tags.Items {
		requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: revision.Name}})
	}
	return requests
}

// ignoreStatusChange returns a predicate that ignores watch events where only the resource status changes; if
// there are any other changes to the resource, the event is not ignored.
// This ensures that the controller doesn't reconcile the entire IstioRevisionTag every time the status of an owned
// resource is updated. Without this predicate, the controller would continuously reconcile the IstioRevisionTag
// because the status.currentMetrics of the HorizontalPodAutoscaler object was updated.
func ignoreStatusChange() predicate.Funcs {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			return specWasUpdated(e.ObjectOld, e.ObjectNew) ||
				!reflect.DeepEqual(e.ObjectNew.GetLabels(), e.ObjectOld.GetLabels()) ||
				!reflect.DeepEqual(e.ObjectNew.GetAnnotations(), e.ObjectOld.GetAnnotations()) ||
				!reflect.DeepEqual(e.ObjectNew.GetOwnerReferences(), e.ObjectOld.GetOwnerReferences()) ||
				!reflect.DeepEqual(e.ObjectNew.GetFinalizers(), e.ObjectOld.GetFinalizers())
		},
	}
}

func specWasUpdated(oldObject client.Object, newObject client.Object) bool {
	// for HPAs, k8s doesn't set metadata.generation, so we actually have to check whether the spec was updated
	if oldHpa, ok := oldObject.(*autoscalingv2.HorizontalPodAutoscaler); ok {
		if newHpa, ok := newObject.(*autoscalingv2.HorizontalPodAutoscaler); ok {
			return !reflect.DeepEqual(oldHpa.Spec, newHpa.Spec)
		}
	}

	// for other resources, comparing the metadata.generation suffices
	return oldObject.GetGeneration() != newObject.GetGeneration()
}

func wrapEventHandler(logger logr.Logger, handler handler.EventHandler) handler.EventHandler {
	return enqueuelogger.WrapIfNecessary(v1alpha1.IstioRevisionTagKind, logger, handler)
}

type NameAlreadyExistsError struct {
	Message string
}

func (err NameAlreadyExistsError) Error() string {
	return err.Message
}

func IsNameAlreadyExistsError(err error) bool {
	if _, ok := err.(NameAlreadyExistsError); ok {
		return true
	}
	return false
}

type ReferenceNotFoundError struct {
	Message string
}

func (err ReferenceNotFoundError) Error() string {
	return err.Message
}

func IsReferenceNotFoundError(err error) bool {
	if _, ok := err.(ReferenceNotFoundError); ok {
		return true
	}
	return false
}