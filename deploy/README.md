# OpenCloudCosts — Deployment

The server runs in HTTP transport mode inside a container and is accessed by your
MCP client over the network — the LLM and the pricing server are completely separate
processes. See [`examples/`](../examples/) for client-side configs that point at it.

---

## Docker Compose

```bash
cd deploy/docker
docker compose up -d
```

The server starts on `http://localhost:8080`. Point your MCP client at
`http://localhost:8080/mcp/v1`.

**Credentials:** copy [`../../.env.example`](../../.env.example) to
`deploy/docker/.env`, fill in the values you need, then uncomment the
relevant `environment:` lines in `docker-compose.yml`. Public AWS and Azure
pricing works with no credentials at all.

```bash
cp ../../.env.example .env
# edit .env, then:
docker compose up -d
```

---

## Kubernetes

### Prerequisites
- `kubectl` configured against your cluster
- Image pull access to `ghcr.io` (public, no auth needed for pulls)

### Quick start

```bash
cd deploy/kubernetes

# 1. Copy the template and fill in your credentials
#    secret.yaml is gitignored — safe to populate with real values
cp secret.yaml.example secret.yaml
vim secret.yaml

# 2. Apply everything
kubectl apply -k .

# 3. Verify
kubectl -n opencloudcosts get pods
kubectl -n opencloudcosts logs -f deployment/opencloudcosts
```

The service is available inside the cluster at
`http://opencloudcosts.opencloudcosts.svc.cluster.local:8080/mcp/v1`.

### Exposing externally

Uncomment `ingress.yaml` in `kustomization.yaml` and update the host/TLS fields.
For Claude.ai web or any remote MCP client, the endpoint must be HTTPS with a
public hostname.

### Credential best practices

| Platform | Preferred approach |
|----------|--------------------|
| EKS | IRSA — annotate the ServiceAccount with an IAM role ARN; no AWS keys needed |
| GKE | Workload Identity — annotate the ServiceAccount; no GCP keys needed |
| AKS | Workload Identity / Managed Identity |
| Other | Populate `secret.yaml`; consider Sealed Secrets or External Secrets Operator |

### Updating the image

```bash
kubectl -n opencloudcosts rollout restart deployment/opencloudcosts
```

Or pin to a specific version in `deployment.yaml`:

```yaml
image: ghcr.io/x7even/opencloudcosts:0.8.8
```

---

## Health check

Both Docker and Kubernetes configs probe `GET /health`. A `200 OK` means the
server is up and all configured providers initialised successfully.

```bash
curl http://localhost:8080/health
```
