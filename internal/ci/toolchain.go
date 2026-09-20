package ci

var checkToolAssets = map[string]ToolAsset{
	"flux":       {"https://github.com/fluxcd/flux2/releases/download/v2.9.5/flux_2.9.5_linux_amd64.tar.gz", "b853df82adfd7736f580692f9f734473d571606307139f8fd20c2a80dd1ff473", "flux"},
	"tailscale":  {"https://pkgs.tailscale.com/stable/tailscale_1.102.4_amd64.tgz", "50748df1045e60b5b695f19f4c56b0da36c019948b440fb456b6584a50f0d8b9", "tailscale_1.102.4_amd64/tailscale"},
	"tailscaled": {"https://pkgs.tailscale.com/stable/tailscale_1.102.4_amd64.tgz", "50748df1045e60b5b695f19f4c56b0da36c019948b440fb456b6584a50f0d8b9", "tailscale_1.102.4_amd64/tailscaled"},
	"actionlint": {"https://github.com/rhysd/actionlint/releases/download/v1.7.12/actionlint_1.7.12_linux_amd64.tar.gz", "8aca8db96f1b94770f1b0d72b6dddcb1ebb8123cb3712530b08cc387b349a3d8", "actionlint"},
	"age":        {"https://github.com/FiloSottile/age/releases/download/v1.3.2/age-v1.3.2-linux-amd64.tar.gz", "cbe24006683f8eb669266162894b9a522a1af52f2665fbc63a4bb032ed26ac10", "age/age"},
	"age-keygen": {"https://github.com/FiloSottile/age/releases/download/v1.3.2/age-v1.3.2-linux-amd64.tar.gz", "cbe24006683f8eb669266162894b9a522a1af52f2665fbc63a4bb032ed26ac10", "age/age-keygen"},
	"sops":       {"https://github.com/getsops/sops/releases/download/v3.13.3/sops-v3.13.3.linux.amd64", "e5bec3346a873ae91d871550f3e698c1aad962aff462a080e40f25fde17fef6b", ""},
	"uv":         {"https://github.com/astral-sh/uv/releases/download/0.12.17/uv-x86_64-unknown-linux-gnu.tar.gz", "fa82fd8dde8e8eefdecada6aa0889666556cfceb690d06e0c3bca49eb3070a63", "uv-x86_64-unknown-linux-gnu/uv"},
	"tofu":       {"https://github.com/opentofu/opentofu/releases/download/v1.12.6/tofu_1.12.6_linux_amd64.tar.gz", "50a6106fa4de523d09c87af85f3db1dd47535fc005727fdca6852146476b88ec", "tofu"},
	"helm":       {"https://get.helm.sh/helm-v4.3.0-linux-amd64.tar.gz", "86584a54def73570558f66f5111cc53dfed56689637ae32c1201205d494f54fb", "linux-amd64/helm"},
	"kubectl":    {"https://dl.k8s.io/release/v1.36.3/bin/linux/amd64/kubectl", "ebbd080e7c2e275093b55915722043257eb24004363e20acb3c4d71919f88336", ""},
}

func toolAsset(name string) (ToolAsset, bool) {
	if asset, ok := ToolAssets[name]; ok {
		return asset, true
	}
	asset, ok := checkToolAssets[name]
	return asset, ok
}
