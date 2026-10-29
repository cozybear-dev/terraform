#!/usr/bin/env python3
"""Generate a configuration using the real vault and google providers.

Vault resources talk to a local Vault dev server; Google resources talk to
gcpmock.py through the provider's custom endpoints.

usage: gen4.py OUTDIR MODULES PER_MODULE

With COPIES=1 in the environment, each module call has a module directory
of its own, as modules from a registry or a bucket do once installed,
rather than all of them sharing one local module.

With BLOB_KB=K, every Vault secret also holds the same K KiB of text, which
makes the state larger without adding objects.
"""
import hashlib, os, random, sys
out, modules, n = sys.argv[1], int(sys.argv[2]), int(sys.argv[3])
blob_kb = int(os.environ.get("BLOB_KB", "0"))
random.seed(5)

APP = r'''
terraform {
  required_providers {
    google = { source = "hashicorp/google" }
    vault  = { source = "hashicorp/vault" }
  }
}

variable "name" { type = string }
variable "n" { type = number }
variable "mount" { type = string }
variable "upstream" {
  type    = map(string)
  default = {}
}
%(blob_variable)s
locals {
  keys = [for i in range(var.n) : format("k%03d", i)]
}

resource "google_pubsub_topic" "t" {
  for_each = toset(local.keys)
  name     = "${var.name}-${each.key}"
  labels = {
    module = var.name
    key    = each.key
    tier   = "bench"
  }
  message_retention_duration = "86600s"
}

resource "google_pubsub_subscription" "s" {
  for_each                   = google_pubsub_topic.t
  name                       = "${var.name}-${each.key}-sub"
  topic                      = each.value.id
  ack_deadline_seconds       = 20
  message_retention_duration = "1200s"
  labels = {
    module   = var.name
    upstream = "u${length(var.upstream)}"
  }
  expiration_policy {
    ttl = "300000.5s"
  }
  retry_policy {
    minimum_backoff = "10s"
  }
}

resource "google_secret_manager_secret" "x" {
  count     = floor(var.n / 2)
  secret_id = "${var.name}-secret-${count.index}"
  labels    = { module = var.name }
  replication {
    auto {}
  }
}

data "google_iam_policy" "p" {
  count = max(1, floor(var.n / 10))
  binding {
    role    = "roles/pubsub.publisher"
    members = ["serviceAccount:${var.name}-${count.index}@tfperf.iam.gserviceaccount.com"]
  }
  binding {
    role    = "roles/pubsub.subscriber"
    members = ["group:${var.name}@example.com", "user:someone-${count.index}@example.com"]
  }
}

data "vault_policy_document" "d" {
  for_each = toset(local.keys)
  rule {
    path         = "${var.mount}/data/${var.name}/${each.key}"
    capabilities = ["read", "list"]
    description  = "read ${each.key}"
  }
  rule {
    path         = "${var.mount}/metadata/${var.name}/*"
    capabilities = ["list"]
  }
}

resource "vault_policy" "p" {
  for_each = toset(local.keys)
  name     = "${var.name}-${each.key}"
  policy   = data.vault_policy_document.d[each.key].hcl
}

resource "vault_kv_secret_v2" "s" {
  for_each = toset(local.keys)
  mount    = var.mount
  name     = "${var.name}/${each.key}"
  data_json = jsonencode({
    topic        = google_pubsub_topic.t[each.key].id
    subscription = google_pubsub_subscription.s[each.key].id
    policy       = vault_policy.p[each.key].name
    iam          = data.google_iam_policy.p[0].policy_data
    upstream     = var.upstream%(blob_entry)s
  })
  custom_metadata {
    max_versions = 5
    data = {
      module = var.name
    }
  }
}

output "first" {
  value = google_pubsub_topic.t["k000"].id
}

output "secrets" {
  value = length(vault_kv_secret_v2.s)
}
'''
if blob_kb > 0:
    APP = APP.replace("%(blob_variable)s", '''
variable "blob" {
  type    = string
  default = ""
}
''').replace("%(blob_entry)s", "\n    blob         = var.blob")
else:
    APP = APP.replace("%(blob_variable)s", "").replace("%(blob_entry)s", "")
copies = os.environ.get("COPIES") == "1"
for d in (["app_%03d" % i for i in range(modules)] if copies else ["app"]):
    os.makedirs(os.path.join(out, "modules", d), exist_ok=True)
    open(os.path.join(out, "modules", d, "main.tf"), "w").write(APP)
root = ['''
terraform {
  required_providers {
    google = { source = "hashicorp/google" }
    vault  = { source = "hashicorp/vault" }
  }
}

provider "google" {
  project                        = "tfperf"
  region                         = "europe-west1"
  access_token                   = "fake-token"
  pubsub_custom_endpoint         = "http://127.0.0.1:8766/v1/"
  secret_manager_custom_endpoint = "http://127.0.0.1:8766/v1/"
}

provider "vault" {
  address          = "http://127.0.0.1:8200"
  token            = "tfperf-root"
  skip_child_token = true
}

variable "bump" {
  type    = string
  default = "0"
}

resource "vault_mount" "kv" {
  path = "bench"
  type = "kv-v2"
}

%(blob_local)sresource "vault_policy" "bump" {
  name   = "bench-bump"
  policy = "path \\"bench/data/bump-${var.bump}\\" { capabilities = [\\"read\\"] }"
}
''']
# The text is hex digits of a chain of digests, the same on every run.
blob, h = "", b"tfperf"
while len(blob) < blob_kb * 1024:
    h = hashlib.sha256(h).digest()
    blob += h.hex()
blob_local = 'locals {\n  blob = "%s"\n}\n\n' % blob[:blob_kb * 1024] if blob_kb > 0 else ""
root[0] = root[0].replace("%(blob_local)s", blob_local)
layer = max(1, int(modules ** 0.5))
for i in range(modules):
    deps = []
    if i >= layer:
        cand = list(range(max(0, (i // layer - 2) * layer), (i // layer) * layer))
        deps = sorted(set(random.sample(cand, min(3, len(cand)))))
    ups = "\n".join('    app_%03d = module.app_%03d.first' % (d, d) for d in deps)
    root.append('''
module "app_%03d" {
  source = "./modules/%s"
  name   = "app-%03d"
  n      = %d
  mount  = vault_mount.kv.path%s
  upstream = {
%s
  }
}
''' % (i, "app_%03d" % i if copies else "app", i, n, "\n  blob   = local.blob" if blob_kb > 0 else "", ups))
open(os.path.join(out, "main.tf"), "w").write("".join(root))
print("modules=%d instances~=%d" % (modules, modules * (5 * n + n // 2 + max(1, n // 10)) + 2))
