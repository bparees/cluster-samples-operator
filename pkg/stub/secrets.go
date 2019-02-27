package stub

import (
	"fmt"

	operatorsv1api "github.com/openshift/api/operator/v1"
	"github.com/openshift/cluster-samples-operator/pkg/apis/samples/v1"
	"github.com/openshift/cluster-samples-operator/pkg/cache"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	coreosPullSecretNamespace = "kube-system"
	coreosPullSecretName      = "coreos-pull-secret"
)

func (h *Handler) copyDefaultClusterPullSecret(secret *corev1.Secret) error {
	var err error
	if secret == nil {
		secret, err = h.secretclientwrapper.Get(coreosPullSecretNamespace, coreosPullSecretName)
		if err != nil {
			return err
		}
		if secret == nil {
			return nil
		}
	}
	logrus.Printf("Copying secret %s from the %s namespace into the operator's namespace", coreosPullSecretName, coreosPullSecretNamespace)
	secretToCreate := corev1.Secret{}
	secret.DeepCopyInto(&secretToCreate)
	secretToCreate.Name = v1.SamplesRegistryCredentials
	secretToCreate.Namespace = ""
	secretToCreate.ResourceVersion = ""
	secretToCreate.UID = ""
	secretToCreate.Annotations = make(map[string]string)
	secretToCreate.Annotations[v1.SamplesVersionAnnotation] = h.version
	_, err = h.secretclientwrapper.Create("openshift", &secretToCreate)
	if kerrors.IsAlreadyExists(err) {
		_, err = h.secretclientwrapper.Update("openshift", &secretToCreate)
	}
	return err
}

func secretsWeCareAbout(secret *corev1.Secret) bool {
	kubeSecret := secret.Name == coreosPullSecretName && secret.Namespace == coreosPullSecretNamespace
	openshiftSecret := secret.Name == v1.SamplesRegistryCredentials && secret.Namespace == "openshift"
	return kubeSecret || openshiftSecret
}

func (h *Handler) manageDockerCfgSecret(deleted bool, cfg *v1.Config, secret *corev1.Secret) error {
	if !secretsWeCareAbout(secret) {
		return nil
	}

	switch secret.Name {
	case v1.SamplesRegistryCredentials:
		// we always want this secret so if deleted recreate
		if deleted {
			err := h.copyDefaultClusterPullSecret(nil)
			if err != nil {
				if kerrors.IsNotFound(err) {
					// if we get not found that means the coreos pull secret is gone so just return;
					// we'll recreate when it is recreated
					h.GoodConditionUpdate(cfg, corev1.ConditionFalse, v1.ImportCredentialsExist)
					return nil
				}
				return err
			}
			h.GoodConditionUpdate(cfg, corev1.ConditionTrue, v1.ImportCredentialsExist)
			return nil
		}

	case coreosPullSecretName:
		// if kube-system deleted, we'll delete ours
		if deleted {
			err := h.secretclientwrapper.Delete("openshift", v1.SamplesRegistryCredentials, &metav1.DeleteOptions{})
			if err != nil && !kerrors.IsNotFound(err) {
				return err
			}
			logrus.Printf("registry dockerconfig secret %s was deleted from the %s namespacae so deleted secret %s in the openshift namespace", secret.Name, secret.Namespace, v1.SamplesRegistryCredentials)
			h.GoodConditionUpdate(cfg, corev1.ConditionFalse, v1.ImportCredentialsExist)
			return nil
		}
		err := h.copyDefaultClusterPullSecret(secret)
		if err == nil {
			h.GoodConditionUpdate(cfg, corev1.ConditionTrue, v1.ImportCredentialsExist)
		}
		return err

	}

	return nil
}

// WaitingForCredential determines whether we should proceed with processing the sample resource event,
// where we should *NOT* proceed if we are RHEL and using the default redhat registry;  The return from
// this method is in 2 flavors:  1) if the first boolean is true, tell the caller to just return nil to the sdk;
// 2) the second boolean being true means we've updated the Config with cred exists == false and the caller should call
// the sdk to update the object
func (h *Handler) WaitingForCredential(cfg *v1.Config) (bool, bool) {
	// if trying to do rhel to the default registry.redhat.io registry requires the secret
	// be in place since registry.redhat.io requires auth to pull; since it is not ready
	// log error state
	if cfg.ClusterNeedsCreds() {
		cred := cfg.Condition(v1.ImportCredentialsExist)
		// - if import cred is false, and the message is empty, that means we have NOT registered the error, and need to do so
		// - if cred is false, and the message is there, we can just return nil to the sdk, which "true" for the boolean return value indicates;
		// not returning the same error multiple times to the sdk avoids additional churn; once the secret comes in, it will update the Config
		// with cred == true, and then we'll get another Config event that will trigger config processing
		if len(cred.Message) > 0 {
			return true, false
		}
		err := fmt.Errorf("Cannot create rhel imagestreams to registry.redhat.io without the credentials being available")
		h.processError(cfg, v1.ImportCredentialsExist, corev1.ConditionFalse, err, "%v")
		return true, true
	}

	// this is either centos, or the cluster admin is using their own registry for rhel content, so we do not
	// enforce the need for the credential
	return false, false
}

func (h *Handler) processSecretEvent(cfg *v1.Config, dockercfgSecret *corev1.Secret, event v1.Event) error {
	// if the secret event gets through while we are creating samples, it will
	// lead to a conflict when updating in progress to true in the initial create
	// loop, which can lead to an extra cycle of creates as we'll return an error there and retry;
	// so we check on local flag for creations in progress, and force a retry of the secret
	// event; similar to what we do in the imagestream/template watches
	if cache.UpsertsAmount() > 0 {
		return fmt.Errorf("retry secret event because in the middle of an sample upsert cycle")
	}

	removedState := false
	switch cfg.Spec.ManagementState {
	case operatorsv1api.Removed:
		// so our current recipe to switch to rhel is to
		// - mark mgmt state removed
		// - after that complete, edit again, mark install type to rhel and mgmt state to managed
		// but what about the secret needed for rhel ... do we force the user to create the secret
		// while still in managed/centos state?  Even with that, the "removed" action removes the
		// secret the operator creates in the openshift namespace since it was owned/created by
		// the operator
		// So we allow the processing of the secret event while in removed state to
		// facilitate the switch from centos to rhel, as necessitating use of removed as the means for
		// changing from centos to rhel since  we allow changing the distribution once the samples have initially been created
		logrus.Printf("processing secret watch event while in Removed state; deletion event: %v", event.Deleted)
		removedState = true
	case operatorsv1api.Unmanaged:
		logrus.Debugln("Ignoring secret event because samples resource is in unmanaged state")
		return nil
	case operatorsv1api.Managed:
		logrus.Printf("processing secret watch event while in Managed state; deletion event: %v", event.Deleted)
	default:
		logrus.Printf("processing secret watch event like we are in Managed state, even though it is set to %v; deletion event %v", cfg.Spec.ManagementState, event.Deleted)
	}
	deleted := event.Deleted
	if dockercfgSecret.Namespace == "openshift" {
		if !deleted {
			if dockercfgSecret.Annotations != nil {
				_, ok := dockercfgSecret.Annotations[v1.SamplesVersionAnnotation]
				if ok {
					// this is just a notification from a prior upsert
					logrus.Println("creation/update of credential in openshift namespace recognized")
					if !cfg.ConditionTrue(v1.ImportCredentialsExist) {
						h.GoodConditionUpdate(cfg, corev1.ConditionTrue, v1.ImportCredentialsExist)
						logrus.Printf("CRDUPDATE switching import cred to true following openshift namespace event")
						return h.crdwrapper.UpdateStatus(cfg)
					}
					return nil
				}
			}
			// not foolproof protection of course, but the lack of the annotation
			// means somebody tried to create our credential in the openshift namespace
			// on there own ... we are not allowing that
			err := fmt.Errorf("the samples credential was created/updated in the openshift namespace without the version annotation")
			return h.processError(cfg, v1.ImportCredentialsExist, corev1.ConditionUnknown, err, "%v")
		}

		// if deleted, but import credential == true, that means somebody deleted the credential in the openshift
		// namespace; we don't like that either, and will
		// recreate; but we have to account for the fact that on a valid delete/remove, the secret deletion occurs
		// before the updating of the samples resource, so we employ a short term retry
		if cfg.ConditionTrue(v1.ImportCredentialsExist) {
			if h.secretRetryCount < 3 {
				err := fmt.Errorf("retry on credential deletion in the openshift namespace to make sure the operator deleted it")
				h.secretRetryCount++
				return err
			}
		}
		if removedState {
			logrus.Println("deletion of credential in openshift namespace for removed state recognized")
			h.GoodConditionUpdate(cfg, corev1.ConditionFalse, v1.ImportCredentialsExist)
			logrus.Printf("CRDUPDATE secret deletion recognized")
			return h.crdwrapper.UpdateStatus(cfg)
		}
		// otherwise move on to possibly recreate via manageDockerCfgSecret call below
	}
	h.secretRetryCount = 0
	if removedState {
		// ignore any stray non-delete events while in removed state
		return nil
	}
	beforeStatus := cfg.Condition(v1.ImportCredentialsExist).Status
	err := h.manageDockerCfgSecret(deleted, cfg, dockercfgSecret)
	if err != nil {
		h.processError(cfg, v1.ImportCredentialsExist, corev1.ConditionUnknown, err, "%v")
		// will not return secret error for immediate retry, but check for unknown in config event path
		// or if cfg update fails, that will initiate a retry on the secret event
		logrus.Printf("CRDUPDATE event secret update error")
		// update the error even if we are in error before (updated times and in case error changes)
	} else {
		afterStatus := cfg.Condition(v1.ImportCredentialsExist).Status
		if beforeStatus == afterStatus {
			return nil
		}
		logrus.Printf("CRDUPDATE event secret update")
	}
	// flush the status changes generated by the processing
	return h.crdwrapper.UpdateStatus(cfg)

}
