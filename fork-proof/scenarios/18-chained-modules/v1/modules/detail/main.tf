variable "name" {
  type = string
}

variable "value" {
  type = string
}

locals {
  doubled = "${var.value}${var.value}"
}

resource "fp_thing" "detail" {
  name  = var.name
  value = local.doubled
}

output "late" {
  value = fp_thing.detail.late
}
