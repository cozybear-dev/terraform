variable "name" {
  type = string
}

variable "leaves" {
  type = number
}

variable "value" {
  type = string
}

resource "fp_thing" "own" {
  name  = "${var.name}-own"
  value = var.value
}

module "leaf" {
  source = "../leaf"
  count  = var.leaves
  prefix = "${var.name}-${count.index}"
  settings = {
    value = "${fp_thing.own.late}/${count.index}"
    ports = [for p in range(count.index + 1) : 8000 + p]
  }
}

output "leaf_lates" {
  value = module.leaf[*].late
}

output "summary" {
  value = {
    own   = fp_thing.own.id
    about = [for l in module.leaf : l.about]
  }
}
