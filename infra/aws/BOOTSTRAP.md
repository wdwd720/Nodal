# AWS BOOTSTRAP — Path B

The steps between an AWS account and a `terraform apply`, in order, with who
must perform each and why.

**Account:** `049286562577`. **Region:** `us-east-2` (the region both existing
CLI profiles use; the whole stack should live in one region).

---

## 0. The one thing that must change first

The `default` CLI profile is `arn:aws:iam::049286562577:root`, and its session
has expired. **Terraform must not run as root.** Root cannot be constrained by a policy, cannot be revoked
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
administrator, because `bdg-deployer` has no IAM permissions at all — it cannot
even list roles, so this document cannot say whether any other administrator
identity exists.

### The authentication model already on this machine

`~/.aws/credentials` is empty. Both profiles in `~/.aws/config` are
`login_session` profiles:

```
[default]
login_session = arn:aws:iam::049286562577:root
[profile bdg-deployer]
login_session = arn:aws:iam::049286562577:user/bdg-deployer
```

That is `aws login`, which mirrors a console sign-in into short-lived CLI
credentials and refreshes them while the refresh token lasts. **No long-lived
access key exists on this machine, and none needs to.** Everything below keeps
that true.

### What gets created

Three objects, and one of them is only a trust relationship.

| | Name | Why |
|---|---|---|
| Managed policy | `nodal-terraform` | `infra/aws/nodal-terraform-policy.json`, unchanged |
| IAM role | `nodal-terraform` | Holds the policy. Terraform runs as this and nothing else |
| IAM user | `nodal-deploy` | Console sign-in only. Its **entire** permission set is `sts:AssumeRole` on the role above |

`nodal-deploy` exists because `aws login` mirrors a *console* session, and only
a user, root or Identity Center can sign into a console. It is a human
credential (a password, ideally with MFA), not a machine credential: it holds no
access key and can do exactly one thing, which is become the role.

The role is where the permissions live, and its credentials are one-hour STS
tokens. Nothing durable is ever written to disk.

Steps, signed in as root or an administrator:

1. IAM → Policies → Create policy → JSON, paste
   `infra/aws/nodal-terraform-policy.json`, name it **`nodal-terraform`**.
2. IAM → Roles → Create role → Custom trust policy:
   ```json
   {"Version":"2012-10-17","Statement":[{"Effect":"Allow",
    "Principal":{"AWS":"arn:aws:iam::049286562577:user/nodal-deploy"},
    "Action":"sts:AssumeRole"}]}
   ```
   Attach the `nodal-terraform` policy. Name it **`nodal-terraform`**. Set the
   maximum session duration to 1 hour.
3. IAM → Users → Create user **`nodal-deploy`**, **console access enabled**, no
   access key. Attach one inline policy and nothing else:
   ```json
   {"Version":"2012-10-17","Statement":[{"Effect":"Allow",
    "Action":"sts:AssumeRole",
    "Resource":"arn:aws:iam::049286562577:role/nodal-terraform"}]}
   ```
4. Enable MFA on `nodal-deploy`.

Then, in a terminal:

```
aws configure set region us-east-2 --profile nodal-deploy
aws configure set login_session arn:aws:iam::049286562577:user/nodal-deploy --profile nodal-deploy
aws configure set region us-east-2 --profile nodal-terraform
aws configure set role_arn arn:aws:iam::049286562577:role/nodal-terraform --profile nodal-terraform
aws configure set source_profile nodal-deploy --profile nodal-terraform

aws login --profile nodal-deploy
aws sts get-caller-identity --profile nodal-terraform
```

The last line must print the **role** ARN. If the CLI refuses a `login_session`
profile as a `source_profile`, that is the one thing in this design that has not
been proven on this machine, and the fallback is stated below rather than
discovered later.

### If the role cannot be assumed from an `aws login` profile

Then create `nodal-terraform` as a console-only **user** with the policy
attached directly, and no access key:

```
aws configure set login_session arn:aws:iam::049286562577:user/nodal-terraform --profile nodal-terraform
aws login --profile nodal-terraform
```

This is second best and the difference is worth naming. It keeps every property
that matters — short-lived credentials, no access key, no root, no reach into
`bdg-deployer` — and loses one: there is no assume-role boundary, so the
permissions are attached to something that can sign into a console rather than
to something that must be deliberately assumed.

**A long-lived access key is not on this list.** It would be needed only if
`aws login` did not work at all, which it demonstrably does: `bdg-deployer`
authenticates that way today.

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
- Service-linked roles are allowed only for the six AWS services that require
  them.

**ElastiCache was added on 2026-09-08**, because F-82 made a shared rate-limit
store a production requirement and the policy had no `elasticache:*` at all —
the first apply would have failed on the subnet group. The document is now
**5,786 characters excluding whitespace against IAM's 6,144 limit**, so it is
close to needing to be split into two managed policies. Whoever adds the next
service should check that number before assuming there is room.

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
| `nodal-terraform` policy, role and the `nodal-deploy` console user | admin | step 1 |
| Authenticated session for it | admin | step 1 |
| State bucket | nodal-terraform | step 2 |
| `backend.hcl` from the example | engineering | after step 2 |
| ACM certificate for `api-nodal.actorvia.xyz` | nodal-terraform + GoDaddy | step 3 |
| Auth0 tenant, client id and secret | owner | `docs/operations/IDENTITY_PROVIDER.md` |
| A Path B Terraform environment | engineering | **not written** — see below |
| Container images in ECR | engineering | none exist; no GitHub repository exists |
| ElastiCache Redis for the API's rate-limit counters | nodal-terraform | required since F-82; see section 5 |

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

One finding came out of the audit, and it turned out to be on the Path B
critical path after all: **nothing in the repository constructed a Redis
client.** `ratelimit.NewRedisStore` had no caller and `cmd/api` chose the
in-memory store, so with `api_autoscaling.min_capacity = 3` every rate limit was
three times looser than configured and twelve times at maximum capacity.

That is now fixed (F-82), and it changes what Path B has to provision.
`CP_RATELIMIT_BACKEND` selects where the counters live, `config.Validate`
refuses the per-process store for an HTTP binary in STAGING and PROD, and
`cmd/api` refuses to start if the Redis it was told to use does not answer
`PING`. So:

**Path B needs ElastiCache after all, or the API must run as a single task.**
The `redis` Terraform module already exists and is not large -- one
`cache.t4g.micro` replication group is a few dollars a month, against the
alternative of an API whose published rate limit is not the one it enforces.
The single-task alternative is real but is a different decision: it removes the
rolling-deploy and availability properties Path B was chosen to keep.

The audit-worker's S3 Object Lock archive remains **open and unfixed**:
`buildArchive` returns a filesystem archive when `CP_AUDIT_ARCHIVE_DIR` is set
and nil otherwise, so the WORM guarantee the audit bucket exists for is not
wired. `cmd/audit-worker` is not part of the minimal Path B deployment, so it
does not block an apply -- but it is not done, and nothing here should be read
as saying it is.
