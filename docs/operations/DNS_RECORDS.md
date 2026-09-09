# DNS records for api-nodal.actorvia.xyz

`actorvia.xyz` is registered at GoDaddy and served by `ns03/ns04.domaincontrol.com`.
Every record below is added there. **Nothing here touches the apex or `www`**, so
the Vercel site and the live Actorvia Stripe webhook are unaffected.

## Ordering, and why it is not negotiable

`actorvia.xyz` sends

```
Strict-Transport-Security: max-age=63072000; includeSubDomains; preload
```

`includeSubDomains` with `preload` means a browser refuses plain HTTP on **any**
subdomain and offers no click-through on a bad certificate. A hostname published
before its certificate exists is a name that fails closed for every visitor,
with no way to bypass it.

So: record 1 first, wait for ISSUED, then record 2. Record 1 does not make the
hostname resolve; it only proves ownership to ACM.

## Record 1 — ACM validation (add now)

Requested 2026-09-09.
`arn:aws:acm:us-east-2:049286562577:certificate/80b0695b-ab4a-490b-a39d-56c04bf30e5f`

| Field | Value |
|---|---|
| Type | `CNAME` |
| Name | `_2beed284bc59a86fc4df3be4a9968e94.api-nodal` |
| Value | `_076b2b471f1078e48f00a7527dd9264a.jkddzztszm.acm-validations.aws` |
| TTL | 600 (or GoDaddy's default) |

GoDaddy appends the zone, so the Name field is written **without**
`.actorvia.xyz`. The fully qualified record is
`_2beed284bc59a86fc4df3be4a9968e94.api-nodal.actorvia.xyz`.

This record points at ACM. It is not a credential and it grants nothing: its
only function is to prove to Amazon that whoever controls this zone asked for
the certificate. It stays in place for the life of the certificate, because ACM
re-validates on renewal and a removed record eventually stops renewal.

Check it with:

```
nslookup -type=CNAME _2beed284bc59a86fc4df3be4a9968e94.api-nodal.actorvia.xyz 8.8.8.8
aws acm describe-certificate --certificate-arn <arn> --profile nodal-terraform --region us-east-2 --query Certificate.Status
```

## Record 2 — the hostname itself (add only after ISSUED)

The load balancer does not exist until Terraform applies, so its DNS name is not
known yet. It comes from the `alb_dns_name` output.

| Field | Value |
|---|---|
| Type | `CNAME` |
| Name | `api-nodal` |
| Value | the ALB DNS name, e.g. `nodal-prod-alb-1234567890.us-east-2.elb.amazonaws.com` |
| TTL | 600 |

A `CNAME` rather than an `A` record: the ALB's addresses change, and the name is
the stable thing. It is also why this can be deleted later without leaving a
dangling address.

## What is deliberately absent

No record for the apex, no record for `www`, and no change to the nameservers.
The existing site and the existing Stripe webhook on `actorvia.xyz` are
untouched by everything in this file.
