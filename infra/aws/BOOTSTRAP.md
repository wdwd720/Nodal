# AWS BOOTSTRAP — Path B

The steps between an AWS account and a `terraform apply`, in order, with who
must perform each and why.

**Account:** `049286562577`. **Region:** `us-east-2` (the region both existing
CLI profiles use; the whole stack should live in one region).

---

## 0. The one thing that must change first

The `default` CLI profile is `arn:aws:iam::049286562577:root`. **Terraform must
not run as root.** Root cannot be constrained by a policy, cannot be revoked
without changing the account password, and leaves an audit trail that says only
"the account owner did it".

The other existing profile, `bdg-deployer`, is scoped to a different project.
It is denied on EC2, S3, RDS, ECS, ACM, Secrets Manager, ELB, Route 53 and IAM,
so it cannot be used here and should not be widened — widening it would give
that project's credentials reach over this one.

**A dedicated identity is step 1, and it is the only step that needs an
administrator.**

## 1. Create the Terraform identity — NEEDS AN ADMIN SIGN-IN

Everything here is done once. It needs the account root or an existing
administrator, because `bdg-deployer` has no IAM permissions at all.

1. Sign in to the AWS console as root (or an admin user).
2. IAM → Policies → Create policy → JSON, and paste
   `infra/aws/nodal-terraform-policy.json`. Name it **`nodal-terraform`**.
3. IAM → Users → Create user, name **`nodal-terraform`**, no console access.
4. Attach the `nodal-terraform` policy to it.
5. Enable `aws login` for it, or create an access key **only if** `aws login`
   is unavailable. A long-lived access key is the worse option and should be
   deleted once OIDC deployment exists.

Then, back in a terminal:

```
aws configure set region us-east-2 --profile nodal-terraform
aws login --profile nodal-terraform
aws sts get-caller-identity --profile nodal-terraform
```

### What the policy allows, and what it deliberately does not

It is broad on create-and-destroy for the services the stack provisions,
because Terraform genuinely creates VPCs, load balancers, databases and KMS
keys, and those resources have no name to scope to before they exist.

It is narrow exactly where breadth would be dangerous:

- **IAM is limited to `nodal-*`.** The stack creates its own task roles and
  nothing else. It cannot touch `bdg-deployer`, the root account, or any role
  belonging to the other project.
- **Creating IAM users, access keys and MFA changes is denied outright**, so a
  compromised Terraform credential cannot mint a second one.
- **Lightsail, billing, Organizations and account settings are denied**, so it
  cannot reach the other project or change what the account costs.
- Service-linked roles are allowed only for the five AWS services that require
  them.

## 2. Terraform state bucket — after step 1, and scriptable

Terraform cannot create the bucket that stores its own state, so this is done
once by hand. As `nodal-terraform`:

```
aws s3api create-bucket --bucket nodal-terraform-state-049286562577-us-east-2 \
  --region us-east-2 --create-bucket-configuration LocationConstraint=us-east-2 \
  --profile nodal-terraform

aws s3api put-bucket-versioning --bucket nodal-terraform-state-049286562577-us-east-2 \
  --versioning-configuration Status=Enabled --profile nodal-terraform

aws s3api put-public-access-block --bucket nodal-terraform-state-049286562577-us-east-2 \
  --public-access-block-configuration \
  BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true \
  --profile nodal-terraform

aws s3api put-bucket-encryption --bucket nodal-terraform-state-049286562577-us-east-2 \
  --server-side-encryption-configuration \
  '{"Rules":[{"ApplyServerSideEncryptionByDefault":{"SSEAlgorithm":"AES256"},"BucketKeyEnabled":true}]}' \
  --profile nodal-terraform
```

Versioning matters more than it looks: it is the only way back from a corrupted
or truncated state file.

No DynamoDB table is needed. The backend uses Terraform's native S3 lock file
(`use_lockfile = true`).

## 3. Certificate before DNS

The ordering is forced by the existing site. `actorvia.xyz` sends

```
Strict-Transport-Security: max-age=63072000; includeSubDomains; preload
```

`includeSubDomains` with `preload` means a browser will refuse plain HTTP on
**any** subdomain and will not offer a click-through on a bad certificate. A
DNS record published before a valid certificate exists is a name that fails
closed for every visitor, with no way to bypass it.

So: request the certificate, prove ownership with a DNS validation record, wait
for issuance, and only then publish the record that points the name at the load
balancer.

```
aws acm request-certificate --domain-name api-nodal.actorvia.xyz \
  --validation-method DNS --region us-east-2 --profile nodal-terraform
```

ACM returns a `CNAME` name and value. That record goes into **GoDaddy**, which
is where `actorvia.xyz` is served from (`ns03/ns04.domaincontrol.com`). It is a
validation record only: it points at ACM, not at anything of ours, and it does
not make the hostname resolve.

The record that makes the hostname live is added last, after the load balancer
exists, as a `CNAME` to the ALB's DNS name.

Neither record touches the apex or `www`, so the Vercel site and the existing
Stripe webhook are unaffected.

## 4. What Path B still needs before an apply can succeed

| Item | Who | State |
|---|---|---|
| `nodal-terraform` IAM user and policy | admin | step 1 |
| Authenticated session for it | admin | step 1 |
| State bucket | nodal-terraform | step 2 |
| `backend.hcl` from the example | engineering | after step 2 |
| ACM certificate for `api-nodal.actorvia.xyz` | nodal-terraform + GoDaddy | step 3 |
| Auth0 tenant, client id and secret | owner | `docs/operations/IDENTITY_PROVIDER.md` |
| A Path B Terraform environment | engineering | **not written** — see below |
| Container images in ECR | engineering | none exist; no GitHub repository exists |

## 5. The blocker inside our own code — CLOSED 2026-09-09

A Path B environment cannot simply be `environments/prod` minus some modules,
and the reason is worth stating precisely.

`internal/config` marks `CP_REDIS_URL`, `CP_REDPANDA_BROKERS`,
`CP_CLICKHOUSE_ADDR` and `CP_TEMPORAL_HOST_PORT` as **required** outside LOCAL
and TEST — for every binary. `cmd/api` dials none of them: it links no
ClickHouse, Temporal or Redpanda client at all and chooses an in-memory
rate-limit store over Redis.

So an API-only deployment must either supply four endpoints that will never be
contacted, which is configuration that lies and which the next person will
believe, or the requirement must become conditional on the binary that has the
dependency.

The second was the fix, and it is done. `Load` now takes a `config.Service`,
and a variable belonging to an external dependency is required only of a binary
that declares that dependency. The table lives in `internal/config/service.go`
and is an audit of what each binary constructs, not of what it might one day
want.

Required-ness became conditional; parsing did not. A malformed broker list
still fails closed for every binary, because it is a mistake whoever set it
wants to hear about whether or not this process would have dialled it.

So a Path B apply is no longer blocked on our own code. `cmd/api` starts in
PROD given Postgres, the archive and its own settings, and the workers that
genuinely need Redpanda, ClickHouse or Temporal still refuse to start without
them.

One finding came out of the audit and is recorded rather than fixed: **nothing
in the repository constructs a Redis client.** `ratelimit.NewRedisStore` exists
with no caller and `cmd/api` chooses the in-memory store. That is a defect
rather than a simplification -- with `api_autoscaling.min_capacity = 3`, a
per-task memory store makes every rate limit three times looser than configured
and twelve times at maximum capacity. It is not on the Path B critical path and
it is named so that nobody reads the empty Redis column as a design.
