variable "n" {
  default = 5
}

locals {
  things = {
    alpha = { value = "A", port = 1 }
    beta  = { value = "B", port = 2 }
    gamma = { value = "C", port = 3 }
    "key with space" = { value = "D", port = 4 }
  }
}

resource "fp_thing" "counted" {
  count = var.n
  name  = "counted-${count.index}"
  value = "index ${count.index}"
  num   = count.index
}

resource "fp_thing" "each" {
  for_each = local.things
  name     = "each-${each.key}"
  value    = each.value.value
  tags     = { for i, t in fp_thing.counted : "c${i}" => t.late }
  rule {
    port = each.value.port
    cidr = "192.168.0.0/16"
  }
}

resource "fp_thing" "from_set" {
  for_each = toset([for t in fp_thing.counted : t.name])
  name     = "set-${each.value}"
  value    = fp_thing.each["alpha"].derived
}

output "names" {
  value = [for t in fp_thing.counted : t.id]
}

output "by_key" {
  value = { for k, t in fp_thing.each : k => t.late }
}
