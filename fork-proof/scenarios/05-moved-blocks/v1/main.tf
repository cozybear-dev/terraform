resource "fp_thing" "old" {
  count = 2
  name  = "old-${count.index}"
  value = "kept"
}

resource "fp_thing" "first" {
  name  = "chain"
  value = "kept"
}

resource "fp_thing" "bare" {
  name  = "bare"
  value = "kept"
}

resource "fp_thing" "indexed" {
  count = 1
  name  = "indexed"
  value = "kept"
}

resource "fp_thing" "keyed" {
  for_each = toset(["a", "b"])
  name     = "keyed-${each.key}"
  value    = "kept"
}

module "m" {
  source = "./modules/pair"
  name   = "m"
}

module "many" {
  source = "./modules/pair"
  count  = 2
  name   = "many-${count.index}"
}
