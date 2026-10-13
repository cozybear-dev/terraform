resource "fp_thing" "existing" {
  name  = "existing"
  value = "managed"
}

# Imported exactly as the provider describes it.
resource "fp_thing" "adopted" {
  name  = "adopted-1"
  value = "imported"
}

import {
  to = fp_thing.adopted
  id = "adopted-1"
}

# Imported and changed in the same plan.
resource "fp_thing" "adjusted" {
  name  = "adopted-2"
  value = "changed after import"
  tags  = { imported = "yes" }
}

import {
  to = fp_thing.adjusted
  id = "adopted-2"
}

# Several at once.
locals {
  batch = {
    one = "batch-1"
    two = "batch-2"
  }
}

resource "fp_thing" "batch" {
  for_each = local.batch
  name     = each.value
  value    = "imported"
}

import {
  for_each = local.batch
  to       = fp_thing.batch[each.key]
  id       = each.value
}
