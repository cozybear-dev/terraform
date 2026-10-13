variable "count_a" {
  default = 2000
}

variable "generation" {
  default = 2
}

locals {
  padding = join("", [for i in range(20) : "0123456789"])
}

resource "fp_thing" "a" {
  count = var.count_a
  name  = "a-${count.index}"
  value = "${count.index % 5 == 0 ? var.generation : 1} ${local.padding}"
  tags  = { index = tostring(count.index), parity = count.index % 2 == 0 ? "even" : "odd" }
}

resource "fp_thing" "b" {
  count = 200
  name  = "b-${count.index}"
  value = fp_thing.a[count.index].late
}

output "last" {
  value = fp_thing.a[var.count_a - 1].id
}

resource "fp_thing" "breaks" {
  name   = "breaks"
  value  = fp_thing.a[0].late
  behave = "fail_apply"
}

resource "fp_thing" "never" {
  count = 2
  name  = "never-${count.index}"
  value = fp_thing.breaks.late
}

resource "fp_thing" "regardless" {
  count = 3
  name  = "regardless-${count.index}"
  value = fp_thing.a[count.index + 1].late
}
