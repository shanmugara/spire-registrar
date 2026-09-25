package controller

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	SpireKubeConfigSecret     = "spire-server-kubeconfig"
	SpireKubeConfigDataSecret = "spire-server-kubeconfig-data"
	SpireUserName             = "spire-server"
	SpireKubeConfigAnnotation = "omegahome.net/spire-kubeconfig"
	SpireKubeConfigLabel      = "omegahome.net/cluster-name"
	ClusterIssuerName         = "cluster-ca-issuer"
)

// GetOwnNamespace returns the namespace the controller is running in.
func GetOwnNamespace() (string, error) {
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns, nil
	}
	// Fallback for in-cluster runs where POD_NAMESPACE wasn't set explicitly.
	data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		return "spire", nil
	}
	return string(data), nil
}

func (r *ServiceAccountReconciler) MakeKubeConfig(ctx context.Context, c client.Client) error {
	spireUserSecret := &corev1.Secret{}
	// Get the namespace the controller is operating in and retrieve the kubeconfig Secret from there
	ns, err := GetOwnNamespace()
	if err != nil {
		return fmt.Errorf("failed to get own namespace: %w", err)
	}

	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: SpireKubeConfigSecret}, spireUserSecret); err != nil {
		return fmt.Errorf("failed to get Secret for kubeconfig: %w", err)
	}

	if len(spireUserSecret.Data) == 0 {
		return fmt.Errorf("missing kubeconfig data in Secret %s/%s", ns, SpireKubeConfigSecret)
	}

	configData, err := r.CreateSerializedKubeConfig(ctx, *spireUserSecret)
	if err != nil {
		return fmt.Errorf("failed to create serialized kubeconfig: %w", err)
	}

	return r.WriteSerializedKubeConfig(ctx, configData)
}

func (r *ServiceAccountReconciler) CreateSerializedKubeConfig(ctx context.Context, s v1.Secret) (*clientcmdapi.Config, error) {

	clusterInfo, err := r.GetClusterInfo(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get cluster info: %w", err)
	}

	if len(s.Data) == 0 {
		return nil, fmt.Errorf("missing kubeconfig data in Secret %s/%s", s.Namespace, s.Name)
	}

	server := clusterInfo["clusterName"].(string)
	serverUrl := "https://" + server + ":6443"

	kcfg := clientcmdapi.NewConfig()
	kcfg.Kind = "Config"
	kcfg.APIVersion = "v1"

	kcfg.Clusters[server] = &clientcmdapi.Cluster{
		Server:                   serverUrl,
		CertificateAuthorityData: s.Data["ca.crt"],
	}
	kcfg.AuthInfos[SpireUserName] = &clientcmdapi.AuthInfo{
		ClientKeyData:         s.Data["tls.key"],
		ClientCertificateData: s.Data["tls.crt"],
		Username:              SpireUserName,
	}
	kcfg.Contexts[SpireUserName] = &clientcmdapi.Context{
		Cluster:  server,
		AuthInfo: SpireUserName,
	}
	kcfg.CurrentContext = SpireUserName

	return kcfg, nil

}

func (r *ServiceAccountReconciler) WriteSerializedKubeConfig(ctx context.Context, config *clientcmdapi.Config) error {
	ns, err := GetOwnNamespace()
	if err != nil {
		return fmt.Errorf("failed to get namespace: %w", err)
	}
	kubeConfigBytes, err := clientcmd.Write(*config)
	if err != nil {
		return fmt.Errorf("failed to serialize kubeconfig: %w", err)
	}
	kubeConfig := base64.StdEncoding.EncodeToString(kubeConfigBytes)
	kcSecret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      SpireKubeConfigDataSecret,
			Namespace: ns,
		},
	}

	if _, err := controllerutil.CreateOrPatch(ctx, r.Client, kcSecret, func() error {
		if kcSecret.Data == nil {
			kcSecret.Data = map[string][]byte{}
		}
		kcSecret.Data["kubeconfig"] = []byte(kubeConfig)
		return nil
	}); err != nil {
		return fmt.Errorf("failed to create or patch Secret %s/%s: %w", ns, SpireKubeConfigDataSecret, err)
	}

	return nil
}

func (r *ServiceAccountReconciler) CreateKubeConfigCert(ctx context.Context) error {
	ns, err := GetOwnNamespace()
	if err != nil {
		return fmt.Errorf("failed to get namespace: %w", err)
	}

	clusterInfo, err := r.GetClusterInfo(ctx)
	if err != nil {
		return fmt.Errorf("failed to get cluster info: %w", err)
	}

	// cert-mnager cert request
	kkCert := certmanagerv1.Certificate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      SpireKubeConfigSecret,
			Namespace: ns,
		},
		Spec: certmanagerv1.CertificateSpec{
			Duration:    &metav1.Duration{Duration: 2160 * time.Hour},
			RenewBefore: &metav1.Duration{Duration: 360 * time.Hour},
			IsCA:        false,
			Subject: &certmanagerv1.X509Subject{
				Organizations: []string{"kubeadm:cluster-admins"},
			},
			CommonName: SpireUserName,
			SecretName: SpireKubeConfigSecret,
			Usages: []certmanagerv1.KeyUsage{
				certmanagerv1.UsageClientAuth,
				certmanagerv1.UsageKeyEncipherment,
			},
			PrivateKey: &certmanagerv1.CertificatePrivateKey{
				Algorithm: certmanagerv1.RSAKeyAlgorithm,
				Encoding:  certmanagerv1.PKCS1,
				Size:      2048,
			},
			// Add other necessary fields for the certificate spec
			SecretTemplate: &certmanagerv1.CertificateSecretTemplate{
				Annotations: map[string]string{
					SpireKubeConfigAnnotation: "true",
				},
				Labels: map[string]string{
					SpireKubeConfigLabel: clusterInfo["clusterName"].(string),
				},
			},
			IssuerRef: cmmeta.ObjectReference{
				Name: ClusterIssuerName,
				Kind: "ClusterIssuer",
			},
		},
	}
	if err := r.Client.Create(ctx, &kkCert); err != nil {
		return fmt.Errorf("failed to create Certificate %s/%s: %w", ns, SpireKubeConfigSecret, err)
	}

	return nil
}

func (r *ServiceAccountReconciler) GetKubeConfigCert(ctx context.Context) (*certmanagerv1.Certificate, error) {
	ns, err := GetOwnNamespace()
	if err != nil {
		return nil, fmt.Errorf("failed to get namespace: %w", err)
	}

	kkCert := &certmanagerv1.Certificate{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: SpireKubeConfigSecret}, kkCert); err != nil {
		return nil, fmt.Errorf("failed to get Certificate %s/%s: %w", ns, SpireKubeConfigSecret, err)
	}

	return kkCert, nil
}

// IsCertificateReady reports whether cert's Ready condition is set to True.
func IsCertificateReady(cert *certmanagerv1.Certificate) bool {
	for _, cond := range cert.Status.Conditions {
		if cond.Type == certmanagerv1.CertificateConditionReady {
			return cond.Status == cmmeta.ConditionTrue
		}
	}
	return false
}
