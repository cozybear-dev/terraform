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
