variable "name" {
  type = string
}

variable "below" {
  type = string
}

resource "fp_thing" "tier" {
  count = 2
  name  = "${var.name}-${count.index}"
  value = var.below
}

output "late" {
  value = fp_thing.tier[0].late
}
