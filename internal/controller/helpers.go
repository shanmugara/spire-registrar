package controller

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
