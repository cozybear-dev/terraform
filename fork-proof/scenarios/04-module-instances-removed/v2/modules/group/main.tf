variable "name" {
  type = string
}

variable "size" {
  type    = number
  default = 2
}

resource "fp_thing" "member" {
  count = var.size
  name  = "${var.name}-member-${count.index}"
  value = var.name
}

module "inner" {
  source   = "../inner"
  for_each = toset(["x", "y"])
  name     = "${var.name}-${each.key}"
  parent   = fp_thing.member[0].late
}

output "ids" {
  value = fp_thing.member[*].id
}
