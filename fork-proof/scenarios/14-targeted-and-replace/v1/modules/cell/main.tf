variable "name" {
  type = string
}

variable "value" {
  type = string
}

resource "fp_thing" "cell" {
  count = 2
  name  = "${var.name}-${count.index}"
  value = var.value
}

output "lates" {
  value = fp_thing.cell[*].late
}
