variable "shared" {
  default = "two"
}

module "mid" {
  source   = "./modules/mid"
  for_each = { a = 2, b = 1, c = 3 }
  name     = each.key
  leaves   = each.value
  value    = var.shared
}

resource "fp_thing" "after" {
  name  = "after"
  value = join(",", flatten([for m in module.mid : m.leaf_lates]))
}

output "summaries" {
  value = { for k, m in module.mid : k => m.summary }
}
