# Raw manifests

The Helm chart in `../helm` is the supported path. If you don't use Helm,
render it once and commit the output:

```bash
helm template health-checker ../helm \
  --set config.ldapURL=ldaps://ldap.example.com:636 \
  --set config.ldapBaseDN='dc=example,dc=com' \
  --set ingress.enabled=true \
  --set ingress.hosts[0].host=health-checker.example.com \
  > rendered.yaml
```

Two things the rendered output does **not** include, because they must come
from your secrets engine rather than from source control:

1. **`health-checker-db`** — a Secret with key `url` holding the Postgres
   connection string.
2. **`health-checker-ldap`** — a Secret with key `bindPassword` holding the
   LDAP service-account password.

Create them with External Secrets Operator, the Vault Agent Injector, or the
Secrets Manager CSI driver. An `ExternalSecret` example:

```yaml
apiVersion: external-secrets.io/v1beta1
kind: ExternalSecret
metadata:
  name: health-checker-db
spec:
  refreshInterval: 1h
  secretStoreRef:
    name: vault-backend
    kind: SecretStore
  target:
    name: health-checker-db
  data:
    - secretKey: url
      remoteRef:
        key: secret/health-checker
        property: database_url
```

Provider credentials (`AVI_API_TOKEN`, `AKAMAI_EDGERC_JSON`, per-service CA
bundles) follow the same pattern. Project them into the pod by adding the
Secret to `secrets.extraEnvFrom` in `values.yaml` — the application resolves
them by environment-variable *name*, which is the only thing stored in
Postgres.
