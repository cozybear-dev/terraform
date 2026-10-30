module "counted" {
  source = "./modules/group"
  count  = 4
  name   = "counted-${count.index}"
}

module "keyed" {
  source   = "./modules/group"
  for_each = toset(["red", "green", "blue"])
  name     = "keyed-${each.key}"
  size     = 3
}

module "single" {
  source = "./modules/group"
  name   = "single"
}

resource "fp_thing" "top" {
  name  = "top"
  value = "unrelated"
}
