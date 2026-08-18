# Local deploy playbook

Build the image and run the whole stack on a local Kubernetes cluster via the
Helm chart, end to end. Written against Docker Desktop's built-in Kubernetes;
notes for kind and minikube are inline where they differ.

The fast path, if you just want it running:

```bash
kubectl apply -f deploy/local/postgres.yaml   # after creating the namespace, step 3
make local-up
kubectl -n health-checker port-forward svc/health-checker 8080:80
```

Everything below is that, unpacked, with a verification after each step so a
failure surfaces where it happened rather than three steps later.

---

## What you need

| Tool | Check | Notes |
| --- | --- | --- |
| Docker | `docker version` | Must be running |
| A local cluster | `kubectl config current-context` | `docker-desktop`, `kind-*`, or `minikube` |
| kubectl | `kubectl version --client` | |
| Helm 3 | `helm version --short` | v3.8+ |
| Go 1.25+, Node 20+ | `go version && node --version` | Only for step 1 |

Enable Kubernetes in Docker Desktop under **Settings → Kubernetes → Enable
Kubernetes** if `current-context` comes back empty.

---

## 1. Verify the source builds

Do this before involving Kubernetes. A failure here is a code problem; a
failure later is a deployment problem, and separating the two saves time.

```bash
make check
```

Runs `go vet`, `go test`, the chart-migration drift check, and the frontend
build. All four must pass.

---

## 2. Build the container image

```bash
make docker            # docker build -t health-checker-router:dev .
```

The Dockerfile is multi-stage: Node builds the React bundle, Go builds a static
binary, and the result is a distroless image containing neither toolchain. The
API serves the bundle from `/app/web`, so the console and the API come from one
container on one port.

```bash
docker image inspect health-checker-router:dev --format '{{.Size}} bytes'
```

**Docker Desktop** shares its image store with the kubelet, so the image is
already visible to the cluster. On other clusters you must load it:

```bash
kind load docker-image health-checker-router:dev --name <cluster>   # kind
minikube image load health-checker-router:dev                       # minikube
```

Skipping this on kind or minikube produces `ErrImageNeverPull` /
`ImagePullBackOff` at step 5, because `values-local.yaml` deliberately never
pulls from a registry.

---

## 3. Create the namespace and database

```bash
kubectl create namespace health-checker
kubectl apply -f deploy/local/postgres.yaml
kubectl -n health-checker rollout status deploy/postgres --timeout=120s
```

That manifest creates three things: a `postgres` Deployment (single replica,
`emptyDir` storage), a `postgres` Service, and the `health-checker-db` Secret
holding the connection string under the key `url`.

The Secret is the contract between the database and the chart. The chart never
templates a credential — it reads `secrets.databaseSecretName` and mounts that
Secret's key into `DATABASE_URL`, and the migration Job reads the same key as
`GOOSE_DBSTRING`. Swapping in a managed Postgres later means replacing this one
Secret, nothing else.

Storage is `emptyDir`, so the database is wiped when the pod restarts. That is
intentional for local work — `kubectl -n health-checker delete pod -l app=postgres`
is your reset button.

Verify:

```bash
kubectl -n health-checker get pod -l app=postgres
kubectl -n health-checker exec deploy/postgres -- \
  psql -U health_checker -d health_checker -c '\l'
```

---

## 4. Sync migrations into the chart

```bash
make helm-sync-migrations
```

Helm's `.Files` is sandboxed to the chart directory — a template cannot read
`../../db/migrations`, and a parent-relative glob silently matches nothing.
Left unsynced, the migration Job would run against an empty directory, report
success, and leave you with an empty schema and a pod that never passes
readiness.

`db/migrations` stays canonical; `deploy/helm/migrations` is the synced copy the
chart packages. `make check` fails if they drift, and the chart refuses to
render if the copy is missing entirely.

---

## 5. Install the chart

```bash
helm upgrade --install health-checker deploy/helm \
  --namespace health-checker \
  --values deploy/helm/values-local.yaml \
  --wait --timeout 5m
```

Or `make local-up`, which rebuilds the image and syncs migrations first.

`values-local.yaml` sets `fullnameOverride: health-checker`. Without it every
resource would be named `health-checker-health-checker-router`, since the
chart's fullname helper always joins release and chart name. Every `kubectl`
command below assumes the override is in place.

What `values-local.yaml` changes from the production defaults, and why:

| Setting | Local | Reason |
| --- | --- | --- |
| `config.authDisabled` | `true` | No LDAP on a laptop. Every request runs as `dev@localhost` with admin rights |
| `image.repository/tag` | `health-checker-router:dev` | The locally built image, never pulled |
| `replicaCount` | `1` | Nothing to spread across on one node |
| `autoscaling.enabled` | `false` | The HPA needs metrics-server, which dev clusters usually lack |
| `podDisruptionBudget.enabled` | `false` | One replica can never satisfy a PDB |
| `networkPolicy.enabled` | `false` | Docker Desktop's CNI does not enforce them; leaving it on would be theatre |
| `ingress.enabled` | `false` | Reach it by port-forward |

Install order is driven by Helm hooks: the migrations ConfigMap
(`hook-weight: -10`), then the goose Job (`-5`), then the Deployment. The Job is
`pre-install,pre-upgrade`, so the schema is never behind the code that expects
it.

Watch it land:

```bash
kubectl -n health-checker get pods -w
```

Expect the `health-checker-migrate` Job pod to run to `Completed`, then the API
pod to reach `1/1 Running`.

---

## 6. Verify the deployment

```bash
kubectl -n health-checker rollout status deploy/health-checker
kubectl -n health-checker logs job/health-checker-migrate
kubectl -n health-checker logs deploy/health-checker | head -30
```

The migration log should show `OK 00001_init.sql` and `OK 00002_seed.sql`. If it
shows `no migrations to run` on a first install, step 4 did not take.

Confirm the schema landed:

```bash
kubectl -n health-checker exec deploy/postgres -- \
  psql -U health_checker -d health_checker -c '\dt'
```

---

## 7. Reach the console

```bash
kubectl -n health-checker port-forward svc/health-checker 8080:80
```

Open <http://localhost:8080>. You should get the console with all five tabs,
already carrying the seed data — two regions and a handful of services.

Check the API directly too:

```bash
curl -s localhost:8080/healthz                  # liveness, no DB
curl -s localhost:8080/readyz                   # readiness, checks the DB
curl -s localhost:8080/api/health | head -c 400 # composite scores
```

`/readyz` failing while `/healthz` passes always means the pod cannot reach
Postgres — the readiness probe checks the database precisely so a pod in that
state leaves the Service instead of serving errors.

---

## 8. Exercise it

The probes target whatever endpoints the seed data points at, which do not
exist locally, so services will read `unknown` rather than `healthy`. That is
the correct behaviour and worth seeing: unknown is not critical, so no traffic
moves and the fail-open banner appears once quorum is lost.

To watch a real state transition, register a service pointing at something
reachable in-cluster:

```bash
kubectl -n health-checker port-forward svc/health-checker 8080:80 &

curl -s -X POST localhost:8080/api/admin/services \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "self",
    "probeType": "http",
    "role": "monitor_only",
    "regions": [
      {"regionId": "us-east-1", "endpoint": "http://health-checker"},
      {"regionId": "us-west-2", "endpoint": "http://health-checker"}
    ],
    "probeConfig": {"path": "/healthz"}
  }'
```

It probes itself, goes healthy within a few cycles, and appears in Services and
on the Overview trend. It registers as `monitor_only` — a new service cannot
move traffic until you explicitly promote it, which is the point of
[docs/adding-services.md](adding-services.md).

Watch the decisions being made:

```bash
kubectl -n health-checker logs -f deploy/health-checker | grep -E 'status changed|fail-open|weight'
```

---

## 9. Iterating

After a code change:

```bash
make local-up
```

Rebuilds the image, re-syncs migrations, and runs `helm upgrade`. Because the
tag stays `dev`, the Deployment's pod template does not change and Kubernetes
will not restart the pod on its own — force it:

```bash
kubectl -n health-checker rollout restart deploy/health-checker
```

Config-only changes do not need that. The Deployment carries a
`checksum/config` annotation over the rendered ConfigMap, so editing
`values-local.yaml` and re-running `helm upgrade` rolls the pods automatically.

---

## Teardown

```bash
make local-down          # helm uninstall + delete the namespace
```

---

## Troubleshooting

**`Error: execution error at (...): Set config.ldapURL ... or config.authDisabled=true`**

The chart is refusing to render a configuration the API would reject at
startup. You are installing without `-f deploy/helm/values-local.yaml`.

Helm attributes this to `deployment.yaml` rather than `configmap.yaml`, because
the Deployment's `checksum/config` annotation renders the ConfigMap first. The
message is the one that matters, not the file.

**`ImagePullBackOff` or `ErrImageNeverPull`**

The cluster cannot see `health-checker-router:dev`. On kind or minikube, load
the image (step 2). On Docker Desktop, confirm the context is right —
`kubectl config current-context` must be `docker-desktop`, not a remote cluster.

**Migration Job fails with `dial tcp: connect: connection refused`**

Postgres is not up yet, or the Secret's host is wrong. The Job has
`backoffLimit: 3`; it will retry. Check `kubectl -n health-checker get pod -l app=postgres`
and that the `url` in `health-checker-db` names the `postgres` Service.

**Migration Job succeeds but `\dt` shows no tables**

The chart's migration copy is empty or stale. `make helm-sync-migrations`, then
`helm upgrade` again.

**Pod is `Running` but never `Ready`**

`/readyz` is failing, which means the database. Check
`kubectl -n health-checker logs deploy/health-checker` for the connection error,
then verify the Secret:

```bash
kubectl -n health-checker get secret health-checker-db \
  -o jsonpath='{.data.url}' | base64 -d
```

**Console loads but every panel is empty, and the browser console shows 401**

Auth is not actually disabled. Confirm the ConfigMap has the key:

```bash
kubectl -n health-checker get cm health-checker-config -o yaml | grep AUTH
```

**`helm upgrade` reports no changes and the pod keeps the old binary**

Expected with a fixed `dev` tag — see step 9. Either
`kubectl rollout restart`, or tag each build (`make docker TAG=$(git rev-parse --short HEAD)`)
and pass `--set image.tag=...`.

---

## What this is not

Auth is off, so anyone who can reach the Service has admin. The database
password is plaintext in a checked-in manifest and the data is on `emptyDir`.
There is one replica, no PDB, and no NetworkPolicy enforcement.

For a real deployment, use `values.yaml` as the base, configure LDAP, put the
database Secret behind External Secrets or the Vault injector, and point at a
managed Postgres — see [deploy/k8s/README.md](../deploy/k8s/README.md) and
[docs/admin.md](admin.md).
