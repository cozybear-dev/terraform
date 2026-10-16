#!/usr/bin/env python3
"""Generate a configuration using the real vault and google providers.

Vault resources talk to a local Vault dev server; Google resources talk to
gcpmock.py through the provider's custom endpoints.

usage: gen4.py OUTDIR MODULES PER_MODULE
"""
import os, random, sys
out, modules, n = sys.argv[1], int(sys.argv[2]), int(sys.argv[3])
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
    upstream     = var.upstream
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
os.makedirs(os.path.join(out, "modules/app"), exist_ok=True)
open(os.path.join(out, "modules/app/main.tf"), "w").write(APP)
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

resource "vault_policy" "bump" {
  name   = "bench-bump"
  policy = "path \\"bench/data/bump-${var.bump}\\" { capabilities = [\\"read\\"] }"
}
''']
layer = max(1, int(modules ** 0.5))
for i in range(modules):
    deps = []
    if i >= layer:
        cand = list(range(max(0, (i // layer - 2) * layer), (i // layer) * layer))
        deps = sorted(set(random.sample(cand, min(3, len(cand)))))
    ups = "\n".join('    app_%03d = module.app_%03d.first' % (d, d) for d in deps)
    root.append('''
module "app_%03d" {
  source = "./modules/app"
  name   = "app-%03d"
  n      = %d
  mount  = vault_mount.kv.path
  upstream = {
%s
  }
}
''' % (i, i, n, ups))
open(os.path.join(out, "main.tf"), "w").write("".join(root))
print("modules=%d instances~=%d" % (modules, modules * (5 * n + n // 2 + max(1, n // 10)) + 2))
