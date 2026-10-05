# Model serving

| Name | Value |
| --- | --- |
| Public API | `https://llm.fredrir.com/v1` |
| Internal API | `http://litellm.llm.svc.cluster.local:4000/v1` |
| Configuration | [LiteLLM](../platform/components/llm/litellm.yaml) |
| Gateway | LiteLLM 1.103.3; one worker and one replica on `fredrir-04`; 180-second request timeout; outbound public HTTPS |
| Upstream data | Image copies of the model map, Anthropic beta headers, autorouter presets, policy templates and blog posts; `LITELLM_LOCAL_*` |
| Agent Platform | `vertex_ai/*`; project `llunde`, location `global`; Gemini, embeddings and Model Garden MaaS; Claude requires `global_online_prediction_requests_per_base_model` quota |
| Agent Platform identity | `litellm@llunde.iam.gserviceaccount.com`; custom role `projects/llunde/roles/modelGatewayInference` with `aiplatform.endpoints.predict`; JSON key in `AGENT_PLATFORM_CREDENTIALS` |
| DeepSeek | `deepseek/*`; `DEEPSEEK_API_KEY` |
| Alibaba Model Studio | `qwencloud/*`; international `dashscope-intl.aliyuncs.com`; `QWENCLOUD_API_KEY` |
| Model list | `/v1/models` expands wildcards from the image model map; unlisted provider models still route; bare `deepseek-*` entries are listed but not routed |
| Granite | `ibm-granite/granite-docling-258M`; llama.cpp on `fredrir-09`; DocTags output |
| PaddleOCR | `PaddlePaddle/PaddleOCR-VL-1.6`; llama.cpp on `fredrir-04`; task-specific image-region recognition |
| PP-StructureV3 | `ghcr.io/fredrir/pp-structure`; PaddleOCR 3.7.0 CPU on `fredrir-04`; layout blocks, reading order and OCR lines |
| PP-StructureV3 source | [`fredrir/litellm`](https://github.com/fredrir/litellm) `services/pp-structure`; manual `VARIANT=cpu` build pinned by digest; public GHCR package since `llm` has no pull secret |
| Layout route | `/pp-structure/health`, `/pp-structure/v1/layout`; LiteLLM pass-through to `pp-structure:8012`; LiteLLM key required |
| Document parsing | Cropping and result assembly run in the calling pipeline |
| Credentials | SOPS-encrypted master, salt, backend, provider and client keys; `fredrir` and parser keys permit Granite, PaddleOCR and the provider wildcards; parser key also permits `/pp-structure` and has a 150 USD budget per 30 days |
| Key limits | `fredrir` 16 and parser 8 parallel requests; no key RPM or TPM limits; local models bounded by deployment `max_parallel_requests` |
| Parser env | `LITELLM_API_URL` internal API; `LITELLM_API_KEY` from `llunde-pyparser/llm-gateway`; Doppler prod `LITELLM_*` entries are not read by the pods |
| Cloudflare clients | Non-urllib User-Agent required; Browser Integrity Check rejects the Python urllib default; pyparser sends `pyparser/<version>` |
| Cloudflare timeout | Proxied requests return 524 after 125 seconds without a response; only Enterprise can raise it; cluster clients use the internal API with the 180-second gateway timeout |
| Public routes | Exact matches for Swagger at `/`, its assets, `/openapi.json`, models, chat completions, Responses, embeddings, Anthropic messages and token counting, model and model group info, liveliness and the layout route; administration requires port forwarding |
| Database | PostgreSQL 17.10; retained 5 GiB local volume on `fredrir-04` |
| Backup | Daily `platform-backups/llm-database-backup`; encrypted control Restic repository; `llm,postgres` tags |
| Retention | Shared repository maintenance: 7 daily, 4 weekly and 12 monthly snapshots per host and tags |
| Model caches | Independent disposable local volumes; downloads use immutable Hugging Face revisions; PP-StructureV3 weights ship in the image |
| Availability | Single gateway, database and model replicas; upgrades can briefly interrupt requests |
| Additional replicas | Add Redis for shared limits and router state before increasing the gateway replica count |
| Research date | 2026-10-05 |

```sh
export KUBECONFIG="$HOME/.kube/config/infra.yaml"
kubectl -n llm port-forward service/litellm 4000:4000
```

```sh
export LITELLM_API_KEY="$(kubectl -n llm get secret litellm-client -o jsonpath='{.data.LITELLM_API_KEY}' | base64 --decode)"
curl --fail https://llm.fredrir.com/v1/models -H "Authorization: Bearer $LITELLM_API_KEY"
kubectl -n platform-backups create job --from=cronjob/llm-database-backup llm-database-backup-manual
```

```sh
docker build --build-arg VARIANT=cpu -t ghcr.io/fredrir/pp-structure:"$(git -C ~/litellm rev-parse --short HEAD)" ~/litellm/services/pp-structure
docker push ghcr.io/fredrir/pp-structure:"$(git -C ~/litellm rev-parse --short HEAD)"
```

```sh
pbpaste | tr -d '\n' | jq -Rs . | sops set --value-stdin platform/components/llm/gateway.secret.sops.yaml '["stringData"]["DEEPSEEK_API_KEY"]'
pbpaste | tr -d '\n' | jq -Rs . | sops set --value-stdin platform/components/llm/gateway.secret.sops.yaml '["stringData"]["QWENCLOUD_API_KEY"]'
gcloud iam service-accounts keys create key.json --iam-account litellm@llunde.iam.gserviceaccount.com
jq @json key.json | sops set --value-stdin platform/components/llm/gateway.secret.sops.yaml '["stringData"]["AGENT_PLATFORM_CREDENTIALS"]' && rm key.json
```

```sh
export LITELLM_MASTER_KEY="$(kubectl -n llm get secret litellm -o jsonpath='{.data.LITELLM_MASTER_KEY}' | base64 --decode)"
export CLIENT_KEY="$(sops -d --extract '["stringData"]["LITELLM_API_KEY"]' platform/components/llm/client.secret.sops.yaml)"
export PARSER_KEY="$(sops -d --extract '["stringData"]["LITELLM_API_KEY"]' platform/projects/llunde-pyparser/llm-gateway.secret.sops.yaml)"
export MODELS='["ibm-granite/granite-docling-258M", "PaddlePaddle/PaddleOCR-VL-1.6", "vertex_ai/*", "deepseek/*", "qwencloud/*"]'
curl --fail -G localhost:4000/key/info --data-urlencode "key=$PARSER_KEY" -H "Authorization: Bearer $LITELLM_MASTER_KEY"
curl --fail localhost:4000/key/update -H "Authorization: Bearer $LITELLM_MASTER_KEY" -H 'Content-Type: application/json' \
  -d "{\"key\": \"$CLIENT_KEY\", \"models\": $MODELS, \"max_parallel_requests\": 16, \"rpm_limit\": null, \"tpm_limit\": null}"
curl --fail localhost:4000/key/update -H "Authorization: Bearer $LITELLM_MASTER_KEY" -H 'Content-Type: application/json' \
  -d "{\"key\": \"$PARSER_KEY\", \"models\": $MODELS, \"max_parallel_requests\": 8, \"rpm_limit\": null, \"tpm_limit\": null, \"max_budget\": 150, \"budget_duration\": \"30d\", \"metadata\": {\"allowed_passthrough_routes\": [\"/pp-structure\"]}}"
curl --fail https://llm.fredrir.com/pp-structure/health -H "Authorization: Bearer $PARSER_KEY"
```

| Source | Basis |
| --- | --- |
| [LiteLLM production practices](https://docs.litellm.ai/docs/proxy/prod) | Worker count, timeouts, PostgreSQL, persistent salt, resource bounds and Redis when scaling |
| [LiteLLM 1.103.3](https://github.com/BerriAI/litellm/releases/tag/v1.103.3) | Stable patch release; image signature verified with the pinned upstream signing key |
| [PaddleOCR deployment](https://www.paddleocr.ai/main/en/version3.x/pipeline_usage/PaddleOCR-VL.html) | CPU support, llama.cpp serving and separation of the VLM from the document pipeline |
| [LiteLLM pass-through](https://docs.litellm.ai/docs/proxy/pass_through) | Route syntax; 1.103.3 enforces `auth` without a license and requires `allowed_passthrough_routes` per key |
| [LiteLLM Vertex AI](https://docs.litellm.ai/docs/providers/vertex) | `vertex_ai/` authenticates with service accounts, ADC or WIF; API keys only on `gemini/` |
| [LiteLLM DashScope](https://docs.litellm.ai/docs/providers/dashscope) | International endpoint; `qwencloud` outside mainland China |
| [LiteLLM DeepSeek](https://docs.litellm.ai/docs/providers/deepseek) | `deepseek/` prefix |
| [Agent Platform name changes](https://docs.cloud.google.com/gemini-enterprise-agent-platform/vertex-ai-name-changes) | Vertex AI renamed to Gemini Enterprise Agent Platform |
| [Cloudflare 524](https://developers.cloudflare.com/support/troubleshooting/http-status-codes/cloudflare-5xx-errors/error-524/) | 125-second proxy read timeout |
| [PP-StructureV3](https://www.paddleocr.ai/main/en/version3.x/pipeline_usage/PP-StructureV3.html) | Layout detection, reading order and OCR in one CPU pipeline |
| [PaddleOCR-VL-1.6 GGUF](https://huggingface.co/PaddlePaddle/PaddleOCR-VL-1.6-GGUF) | Official model and vision projector |
