variable "name" {
  type = string
}

variable "parent" {
  type = string
}

resource "fp_thing" "inner" {
  name  = "${var.name}-inner"
  value = var.parent
}

resource "fp_thing" "second" {
  name  = "${var.name}-second"
  value = fp_thing.inner.late
}
