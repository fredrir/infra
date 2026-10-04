# Model serving

| Name | Value |
| --- | --- |
| Public API | `https://llm.fredrir.com/v1` |
| Internal API | `http://litellm.llm.svc.cluster.local:4000/v1` |
| Configuration | [LiteLLM](../platform/components/llm/litellm.yaml) |
| Gateway | LiteLLM 1.103.3; one worker and one replica on `fredrir-04`; 180-second request timeout |
| Granite | `ibm-granite/granite-docling-258M`; llama.cpp on `fredrir-09`; DocTags output |
| PaddleOCR | `PaddlePaddle/PaddleOCR-VL-1.6`; llama.cpp on `fredrir-04`; task-specific image-region recognition |
| Document parsing | PaddleOCR layout detection, cropping and result assembly run in the calling pipeline |
| Credentials | SOPS-encrypted master, salt, backend and client keys; parser key permits Granite only |
| Cloudflare clients | OpenAI SDK and curl user agents pass; the default Python urllib user agent is rejected by Browser Integrity Check |
| Public routes | Exact matches for Swagger at `/`, its assets, `/openapi.json`, models, chat completions, Responses and liveliness; administration requires port forwarding |
| Database | PostgreSQL 17.10; retained 5 GiB local volume on `fredrir-04` |
| Backup | Daily `platform-backups/llm-database-backup`; encrypted control Restic repository; `llm,postgres` tags |
| Retention | Shared repository maintenance: 7 daily, 4 weekly and 12 monthly snapshots per host and tags |
| Model caches | Independent disposable local volumes; downloads use immutable Hugging Face revisions |
| Availability | Single gateway, database and model replicas; upgrades can briefly interrupt requests |
| Additional replicas | Add Redis for shared limits and router state before increasing the gateway replica count |
| Research date | 2026-10-04 |

```sh
export KUBECONFIG="$HOME/.kube/config/infra.yaml"
kubectl -n llm port-forward service/litellm 4000:4000
```

```sh
export LITELLM_API_KEY="$(kubectl -n llm get secret litellm-client -o jsonpath='{.data.LITELLM_API_KEY}' | base64 --decode)"
curl --fail https://llm.fredrir.com/v1/models -H "Authorization: Bearer $LITELLM_API_KEY"
kubectl -n platform-backups create job --from=cronjob/llm-database-backup llm-database-backup-manual
```

| Source | Basis |
| --- | --- |
| [LiteLLM production practices](https://docs.litellm.ai/docs/proxy/prod) | Worker count, timeouts, PostgreSQL, persistent salt, resource bounds and Redis when scaling |
| [LiteLLM 1.103.3](https://github.com/BerriAI/litellm/releases/tag/v1.103.3) | Stable patch release; image signature verified with the pinned upstream signing key |
| [PaddleOCR deployment](https://www.paddleocr.ai/main/en/version3.x/pipeline_usage/PaddleOCR-VL.html) | CPU support, llama.cpp serving and separation of the VLM from the document pipeline |
| [PaddleOCR-VL-1.6 GGUF](https://huggingface.co/PaddlePaddle/PaddleOCR-VL-1.6-GGUF) | Official model and vision projector |
