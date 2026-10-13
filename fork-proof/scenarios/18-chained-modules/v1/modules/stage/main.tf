variable "name" {
  type = string
}

variable "upstream" {
  type = map(string)
}

variable "width" {
  type = number
}

variable "salt" {
  type = string
}

locals {
  inputs  = { for k, v in var.upstream : k => "${v}+${var.name}" }
  first   = "${var.name}-${var.salt}"
  second  = upper(local.first)
  third   = "${local.second}/${length(local.inputs)}"
  fourth  = [for i in range(var.width) : "${local.third}#${i}"]
  fifth   = { for i, v in local.fourth : "w${i}" => v }
  summary = join(";", [for k, v in local.inputs : "${k}=${v}"])
}

resource "fp_thing" "work" {
  for_each = local.fifth
  name     = "${var.name}-${each.key}"
  value    = each.value
  tags     = local.inputs
}

module "detail" {
  source = "../detail"
  count  = 2
  name   = "${var.name}-detail-${count.index}"
  value  = fp_thing.work["w0"].late
}

output "values" {
  value = merge({ for k, t in fp_thing.work : "${var.name}.${k}" => t.late }, { "${var.name}.detail" = module.detail[1].late })
}

output "summary" {
  value = local.summary
}

output "third" {
  value = local.third
}
