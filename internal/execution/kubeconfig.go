package execution

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Inline credentials keep ~/.kube/config and credential plugins on the host
// outside the execution boundary. The resource-side RBAC must be read-only.
func materializeKubeconfig(source, dir string) (map[string]string, error) {
	text, ok := os.LookupEnv(source)
	if !ok || text == "" {
		return nil, fmt.Errorf("服务端未配置 kubeconfig 环境变量：%s", source)
	}
	if len(text) > 1<<20 {
		return nil, fmt.Errorf("kubeconfig 超过 1 MiB")
	}
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(text), &node); err != nil {
		return nil, fmt.Errorf("kubeconfig YAML 无效")
	}
	secrets := map[string]string{"KUBECONFIG_CONTENT": text}
	var visit func(*yaml.Node) error
	visit = func(n *yaml.Node) error {
		if n.Kind == yaml.AliasNode {
			return fmt.Errorf("kubeconfig 不支持 YAML 别名")
		}
		if n.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(n.Content); i += 2 {
				key, value := n.Content[i].Value, n.Content[i+1]
				switch key {
				case "exec", "auth-provider", "tokenFile", "client-key", "client-certificate", "certificate-authority":
					return fmt.Errorf("kubeconfig 必须使用内嵌凭据，不允许插件或宿主机文件引用：%s", key)
				case "insecure-skip-tls-verify":
					if value.Value == "true" {
						return fmt.Errorf("kubeconfig 不能关闭 TLS 校验")
					}
				case "token", "password", "client-key-data":
					secrets[fmt.Sprintf("KUBECONFIG_SECRET_%d", len(secrets))] = value.Value
				}
			}
		}
		for _, child := range n.Content {
			if err := visit(child); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(&node); err != nil {
		return nil, err
	}
	clusters := false
	if len(node.Content) == 1 && node.Content[0].Kind == yaml.MappingNode {
		for i := 0; i+1 < len(node.Content[0].Content); i += 2 {
			if node.Content[0].Content[i].Value == "clusters" {
				clusters = true
			}
		}
	}
	if !clusters {
		return nil, fmt.Errorf("kubeconfig 需要 clusters 配置")
	}
	path := filepath.Join(dir, ".kube")
	if err := os.Mkdir(path, 0700); err != nil {
		return nil, fmt.Errorf("创建私有 kubeconfig 目录失败")
	}
	if err := os.WriteFile(filepath.Join(path, "config"), []byte(text), 0600); err != nil {
		return nil, fmt.Errorf("写入私有 kubeconfig 失败")
	}
	return secrets, nil
}
