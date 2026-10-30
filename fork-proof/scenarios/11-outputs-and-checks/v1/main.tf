variable "limit" {
  default = 10
}

module "reporter" {
  source   = "./modules/reporter"
  for_each = toset(["north", "south", "east"])
  name     = each.key
  limit    = var.limit
}

data "fp_data" "probe" {
  input = "probe"
}

check "probe_length" {
  assert {
    condition     = data.fp_data.probe.length == 5
    error_message = "The probe has the wrong length."
  }
}

check "limit_is_small" {
  data "fp_data" "scoped" {
    input = module.reporter["north"].late
  }
  assert {
    condition     = var.limit < 5
    error_message = "The limit is not small."
  }
  assert {
    condition     = data.fp_data.scoped.length > 0
    error_message = "Nothing was read."
  }
}

output "ids" {
  value = { for k, m in module.reporter : k => m.id }
}

output "lates" {
  value = [for m in module.reporter : m.late]
}

output "limit_texts" {
  value = values(module.reporter)[*].limit_text
}

output "nested" {
  value = module.reporter["south"].nested
}

output "constant" {
  value = "never changes"
}

output "number" {
  value = var.limit * 1.5
}
