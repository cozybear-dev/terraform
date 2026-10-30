terraform {
  required_providers {
    fp = {
      source                = "hashicorp/fp"
      configuration_aliases = [fp.other]
    }
  }
}

variable "name" {
  type = string
}

resource "fp_thing" "default" {
  name  = "${var.name}-default"
  value = "default configuration"
}

resource "fp_thing" "other" {
  provider = fp.other
  name     = "${var.name}-other"
  value    = fp_thing.default.late
}

output "late" {
  value = fp_thing.other.late
}
