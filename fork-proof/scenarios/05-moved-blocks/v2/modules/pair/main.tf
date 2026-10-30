variable "name" {
  type = string
}

resource "fp_thing" "left" {
  name  = "${var.name}-left"
  value = "L"
}

resource "fp_thing" "right" {
  name  = "${var.name}-right"
  value = fp_thing.left.late
}
