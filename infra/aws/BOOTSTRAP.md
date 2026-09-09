# AWS BOOTSTRAP — Path B

The steps between an AWS account and a `terraform apply`, in order, with who
must perform each and why.

**Account:** `049286562577`. **Region:** `us-east-2` (the region both existing
CLI profiles use; the whole stack should live in one region).

---

## 0. The one thing that must change first

The `default` CLI profile is `arn:aws:iam::049286562577:root`, and its session
has expired. **Terraform must not run as root.** Root cannot be constrained by
a policy, cannot be revoked without changing the account password, and leaves
an audit trail that says only "the account owner did it".

The other existing profile, `bdg-deployer`, is scoped to a different project.
Confirmed by probing it on 2026-09-08: it is denied on EC2, ELB, RDS,
ElastiCache, ECS, S3, Secrets Manager, ACM, Route 53, CloudWatch Logs, KMS and
IAM, and can read Lightsail alone. It cannot be used here and must not be
widened, because widening it would give that project's credentials reach over
this one.

**A dedicated identity is step 1, and it is the only step that needs an
administrator.**

## 1. Create the Terraform identity — NEEDS AN ADMIN SIGN-IN

Done once. It needs the account root or an existing administrator, because
`bdg-deployer` has no IAM permissions at all.

### No IAM user is created. The chain was tested.

`~/.aws/credentials` is empty. Both profiles in `~/.aws/config` are
`login_session` profiles, which is `aws login`: it mirrors a console sign-in
into short-lived CLI credentials and refreshes them while the refresh token
lasts. No long-lived access key exists on this machine.

The open question was whether those credentials can be the *source* of an
`sts:AssumeRole`. They can. Tested on 2026-09-08 with a profile whose
`source_profile` was the existing `aws login` profile and whose `role_arn`
named a role that does not exist:

```
An error occurred (AccessDenied) when calling the AssumeRole operation:
User: arn:aws:iam::049286562577:user/bdg-deployer is not authorized to
perform: sts:AssumeRole on resource: .../nodal-terraform-does-not-exist-probe
```

The CLI resolved the login session into credentials and reached STS with them.
The only refusal was the IAM authorization on a deliberately absent role, which
is the answer we wanted.

**So the chain is:**

```
administrator browser sign-in
  -> aws login                     (short-lived, refreshed, nothing on disk)
  -> sts:AssumeRole                (trust policy names the administrator)
  -> nodal-terraform role          (1-hour credentials, the four policies below)
  -> terraform
```

No `nodal-deploy` user. No access key. The earlier draft of this document
proposed a console-only IAM user as the source principal; the probe made it
unnecessary, and it is not created.

### Which principal the trust policy names

This is the one decision that cannot be made from here, because `bdg-deployer`
cannot list IAM, Organizations or Identity Center. It is made at bootstrap
from what is actually in the account, in this order:

1. **IAM Identity Center**, if an instance exists. Trust the permission-set
   role ARN. Fully federated, no IAM user anywhere, and the preferred AWS
   model.
2. **An existing non-root administrator IAM user**, if one exists. Trust that
   user's ARN exactly.
3. **Root, narrowed by condition**, if neither exists. `"Principal": {"AWS":
   "arn:aws:iam::049286562577:root"}` in a trust policy means *the whole
   account*, not the root user, so it is paired with a condition that pins it
   to the root user itself:

   ```json
   "Condition": {"ArnEquals": {"aws:PrincipalArn": "arn:aws:iam::049286562577:root"}}
   ```

   This is an interim. It requires a root browser session to refresh the role,
   which should be rare, and it is replaced by option 1 or 2 as soon as one
   exists.

### What gets created

Six documents and one role. Nothing durable, nothing with a password.

| | Name | Size |
|---|---|---|
| Policy | `nodal-terraform-read` | 2,100 of 6,144 |
| Policy | `nodal-terraform-network` | 1,423 of 6,144 |
| Policy | `nodal-terraform-stack` | 5,050 of 6,144 |
| Policy | `nodal-terraform-iam` | 3,046 of 6,144 |
| Policy | `nodal-task-boundary` | 2,094 of 6,144 |
| Role | `nodal-terraform` | max session 1 hour |

The single `nodal-terraform` policy that used to live here reached 5,786 of
6,144 characters and is gone. The split is by blast radius, not by service:

- **read** is every `Describe`, `List` and `Get`. Most of the characters, none
  of the risk, and separating it is what leaves room for conditions on the
  other three.
- **network** is EC2 and VPC, the one place AWS cannot name a resource before
  it exists.
- **stack** is everything the repository names itself.
- **iam** is the smallest and decides whether the other three matter.

`nodal-task-boundary` is not attached to the deployment role. It is the ceiling
on every role the deployment role creates, and `iam:CreateRole` is conditioned
on it.

Steps, signed in as an administrator:

1. IAM → Policies → Create policy → JSON, once per file in `infra/aws/`.
2. IAM → Roles → Create role → Custom trust policy, naming the principal chosen
   above. Attach the four `nodal-terraform-*` policies. Name it
   `nodal-terraform`, maximum session duration 1 hour.

Then, in a terminal:

```
aws configure set region us-east-2 --profile nodal-terraform
aws configure set role_arn arn:aws:iam::049286562577:role/nodal-terraform --profile nodal-terraform
aws configure set source_profile <the administrator profile> --profile nodal-terraform
aws sts get-caller-identity --profile nodal-terraform
```

The last line must print the **role** ARN, not the administrator's.

## 1a. What the policies allow, and what they cannot reach

### The two anchors

Everything below rests on two facts about the Terraform, and both are enforced
in code rather than remembered:

- **Every resource is named `nodal-*`.** `var.project` was `"cp"`, which no
  policy anywhere allowed, so the first apply would have failed on IAM role
  creation. It is now `"nodal"` with a `validation` block that refuses anything
  else.
- **Every resource is tagged `Project=nodal`**, through the provider's
  `default_tags`.

Names carry the ARN scoping. Tags carry the mutation gates for the resources
whose names AWS generates.

### Cross-project isolation, stated as properties

The account also holds a Lightsail project: one Ubuntu instance,
`bdg-protected-backend`, with no VPC peering. Lightsail is denied outright, and
it is a separate service namespace that `ec2:*` cannot reach in any case.

What is **not** known from here is whether anything else lives in the account.
`bdg-deployer` can read Lightsail and nothing else: EC2, ELB, RDS, ElastiCache,
ECS, S3, Secrets Manager, ACM, Route 53, CloudWatch Logs and KMS all return
AccessDenied. So the policies are written to be safe without knowing, which is
the better property anyway:

| Service | How an unrelated resource is kept out of reach |
|---|---|
| EC2 / VPC | Creates are open, because a VPC has no ARN before it exists. **Every** mutation and deletion is gated on `aws:ResourceTag/Project = nodal`. `ec2:CreateTags` is allowed only as part of a create, via `ec2:CreateAction` |
| ELB | ARN-scoped to `loadbalancer/app/nodal-*`, `targetgroup/nodal-*`, listeners under those |
| RDS | ARN-scoped to `db:nodal-*`, `subgrp:nodal-*`, `pg:nodal-*` |
| ElastiCache | ARN-scoped to `replicationgroup:nodal-*`, `subnetgroup:nodal-*`, `parametergroup:nodal-*` |
| ECS | ARN-scoped to `cluster/nodal-*` and `service/nodal-*` |
| S3 | ARN-scoped to `arn:aws:s3:::nodal-*`. The previous policy granted `s3:Get*` on `*`, which was read access to every object in the account |
| Secrets Manager | ARN-scoped to `secret:nodal/*`. Values are never readable: `GetSecretValue` is not granted to the deployment identity at all |
| ACM | Certificate ARNs are server-generated, so `RequestCertificate` is open and delete and tag are gated on `Project=nodal` |
| KMS | `CreateKey` is open for the same reason; every other key action is gated on `Project=nodal` |
| CloudWatch / logs | ARN-scoped to `alarm:nodal-*`, `dashboard/nodal-*` and the four log-group prefixes the modules actually create |
| SNS | ARN-scoped to `nodal-*` |
| ECR | ARN-scoped to `repository/nodal-*` |
| IAM | `role/nodal-*` and `policy/nodal-*` only, under a permissions boundary |
| Route 53 | **Removed entirely.** Nothing in `infra/terraform` uses Route 53; DNS is at GoDaddy. The old policy granted read on it for no reason |

### Two privilege escalations that were open, and are not now

The old policy allowed `iam:CreatePolicyVersion` on `policy/nodal-*`. Its own
policy was named `nodal-terraform`, so a compromised session could have
published a new version of its own permissions granting itself everything.
Every other restriction in this document would have been decorative. There is
now an explicit `Deny` on `iam:*` against `policy/nodal-terraform-*`,
`policy/nodal-task-boundary` and `role/nodal-terraform`.

The second is subtler and needed a change to the Terraform. An identity that
can create a role, write its inline policy and set its trust policy can grant
itself anything in three calls. The fix is a permissions boundary:
`iam:CreateRole` is allowed only when `iam:PermissionsBoundary` equals
`nodal-task-boundary`, that boundary denies `iam:*`, `lightsail:*`,
`organizations:*`, `account:*` and `sts:AssumeRole`, and every
`aws_iam_role` in the modules now sets `permissions_boundary`. Attaching an
AWS-managed policy is denied except for the one the RDS module genuinely uses,
`AmazonRDSEnhancedMonitoringRole`.

### What can still reach an unrelated resource, and why

Stated rather than implied. `test/infra/policy_test.go` fails if this list and
the policies disagree.

- **Read-only metadata across the account.** `ec2:Describe*`,
  `elasticloadbalancing:Describe*`, `rds:Describe*`, `ecs:List*` and the rest
  accept no resource ARN from AWS. They reveal that resources exist and their
  shape. They return no object content and no secret value.
- **Creating into someone else's VPC.** `ec2:CreateSubnet`,
  `ec2:CreateSecurityGroup` and `ec2:CreateVpcEndpoint` take a VPC id, and the
  condition on a create is evaluated against the resource being created, not
  the VPC. So a compromised session could add a subnet or a security group to
  another VPC. It could not attach that security group to anything, because
  every instance and interface action is absent, and it could not modify or
  delete anything already there.
- **Account-wide log delivery.** `logs:CreateLogDelivery` and
  `logs:PutResourcePolicy` have no resource form. WAF logging to CloudWatch
  Logs requires them.
- **A new certificate or KMS key.** Both are server-named, so creation cannot
  be scoped. Neither touches an existing resource.

Nothing on that list mutates or deletes a resource that already exists. That is
the line the policies draw.

### Not yet done

**AWS IAM Access Analyzer has not been run.** `access-analyzer:ValidatePolicy`
is denied to `bdg-deployer`, so it cannot be run before the administrator
session exists. It is the first action of that session, before anything is
created:

```
for f in infra/aws/*-policy.json; do
  aws accessanalyzer validate-policy --policy-type IDENTITY_POLICY \
    --policy-document "file://$f" --region us-east-2 --profile <admin>
done
```

Findings are fixed before the policies are created, not after.

**The Path B environment does not exist yet.** These policies were derived from
the module set in `infra/terraform/modules`, which is what `environments/prod`
composes. A Path B environment is a different composition, and `terraform plan`
is the only real test of whether the permissions are complete. Expect at least
one missing action; the fix is to add it here, scoped, rather than to widen a
statement to `*`.

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
| Five policies, one role, no IAM user | admin | step 1 |
| Authenticated session for it | admin | step 1 |
| State bucket | nodal-terraform | step 2 |
| `backend.hcl` from the example | engineering | after step 2 |
| ACM certificate for `api-nodal.actorvia.xyz` | nodal-terraform + GoDaddy | step 3 |
| Auth0 tenant, client id and secret | owner | `docs/operations/IDENTITY_PROVIDER.md` |
| A Path B Terraform environment | engineering | **not written** — see below |
| Container images in ECR | engineering | none exist; no GitHub repository exists |
| ElastiCache Redis for the API's rate-limit counters | nodal-terraform | required since F-82; see section 5 |
| Access Analyzer run on the five policies | admin | section 1a, before anything is created |

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
