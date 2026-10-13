variable "value" {
  default = "two"
}

resource "fp_thing" "base" {
  name  = "base"
  value = var.value
}

resource "fp_thing" "dependent" {
  name  = "dependent"
  value = fp_thing.base.late
}

resource "fp_thing" "bystander" {
  count = 3
  name  = "bystander-${count.index}"
  value = var.value
}

module "cell" {
  source   = "./modules/cell"
  for_each = toset(["p", "q"])
  name     = "cell-${each.key}"
  value    = var.value
}

resource "fp_thing" "uses_cells" {
  name  = "uses-cells"
  value = join(",", module.cell["p"].lates)
}
