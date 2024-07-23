package hooks

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/go-logr/logr"
	pvcautoresizer "github.com/topolvm/pvc-autoresizer"
	"github.com/topolvm/pvc-autoresizer/internal/runners"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

//+kubebuilder:webhook:path=/pvc/mutate,mutating=true,failurePolicy=fail,sideEffects=None,groups="",resources=persistentvolumeclaims,verbs=create,versions=v1,name=mpersistentvolumeclaim.topolvm.io,admissionReviewVersions={v1}

type persistentVolumeClaimMutator struct {
	apiReader client.Reader
	dec       admission.Decoder
	log       logr.Logger
}

var _ admission.Handler = &persistentVolumeClaimMutator{}

func (m *persistentVolumeClaimMutator) Handle(ctx context.Context, req admission.Request) admission.Response {
	if req.Operation != admissionv1.Create {
		return admission.Allowed("not a Create request")
	}
	pvc := &corev1.PersistentVolumeClaim{}
	if err := m.dec.Decode(req, pvc); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	groupLabelKey, ok := pvc.Annotations[pvcautoresizer.InitialResizeGroupByAnnotation]
	if !ok || groupLabelKey == "" {
		return admission.Allowed("annotation not set")
	}
	group, ok := pvc.Labels[groupLabelKey]
	if !ok || group == "" {
		return admission.Errored(http.StatusBadRequest, fmt.Errorf("no value is set to the label key %s", groupLabelKey))
	}

	storageLimit, err := runners.PvcStorageLimit(pvc)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	if storageLimit.IsZero() {
		return admission.Allowed("ignore the PVC because it has no storage limit annotation")
	}

	pvcList := &corev1.PersistentVolumeClaimList{}
	err = m.apiReader.List(ctx, pvcList, &client.ListOptions{
		Namespace:     pvc.Namespace,
		LabelSelector: labels.SelectorFromSet(map[string]string{groupLabelKey: group}),
	})
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}

	requestedSize := *pvc.Spec.Resources.Requests.Storage()
	newSize := requestedSize
	for _, item := range pvcList.Items {
		if itemSize := item.Spec.Resources.Requests.Storage(); itemSize.Cmp(newSize) > 0 {
			newSize = *itemSize
		}
	}
	if newSize.Cmp(storageLimit) > 0 {
		newSize = storageLimit
	}
	if requestedSize.Cmp(newSize) == 0 {
		return admission.Allowed("PVC request storage size unchanged")
	}
	pvc.Spec.Resources.Requests[corev1.ResourceStorage] = newSize

	m.log.Info("need mutate the PVC size",
		"name", pvc.Name,
		"namespace", pvc.Namespace,
		"from-request", requestedSize.Value(),
		"to-request", pvc.Spec.Resources.Requests.Storage().Value(),
	)
	data, err := json.Marshal(pvc)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	return admission.PatchResponseFromRaw(req.Object.Raw, data)
}

// SetupPersistentVolumeClaimWebhook registers the webhooks for PersistentVolumeClaim
func SetupPersistentVolumeClaimWebhook(mgr manager.Manager, dec admission.Decoder, log logr.Logger) error {
	serv := mgr.GetWebhookServer()
	m := &persistentVolumeClaimMutator{
		apiReader: mgr.GetAPIReader(),
		dec:       dec,
		log:       log,
	}
	serv.Register("/pvc/mutate", &webhook.Admission{Handler: m})
	return nil
}
