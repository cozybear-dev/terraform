variable "prefix" {
  type = string
}

variable "settings" {
  type = object({
    value = string
    ports = list(number)
  })
}

locals {
  full = "${var.prefix}-leaf"
}

resource "fp_thing" "leaf" {
  name  = local.full
  value = var.settings.value
  dynamic "rule" {
    for_each = var.settings.ports
    content {
      port = rule.value
      cidr = "10.1.0.0/16"
    }
  }
}

data "fp_data" "about" {
  input = fp_thing.leaf.late
}

output "late" {
  value = fp_thing.leaf.late
}

output "about" {
  value = data.fp_data.about.result
}
