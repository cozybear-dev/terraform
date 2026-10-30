# Renamed, with all of its instances.
resource "fp_thing" "new" {
  count = 2
  name  = "old-${count.index}"
  value = "kept"
}

moved {
  from = fp_thing.old
  to   = fp_thing.new
}

# Renamed twice, by statements that have to be applied in order.
resource "fp_thing" "third" {
  name  = "chain"
  value = "kept"
}

moved {
  from = fp_thing.second
  to   = fp_thing.third
}

moved {
  from = fp_thing.first
  to   = fp_thing.second
}

# Given a count, and stripped of one, with no statement for either.
resource "fp_thing" "bare" {
  count = 1
  name  = "bare"
  value = "kept"
}

resource "fp_thing" "indexed" {
  name  = "indexed"
  value = "kept"
}

# One instance moved to another key.
resource "fp_thing" "keyed" {
  for_each = toset(["a", "c"])
  name     = each.key == "c" ? "keyed-b" : "keyed-${each.key}"
  value    = "kept"
}

moved {
  from = fp_thing.keyed["b"]
  to   = fp_thing.keyed["c"]
}

# A module call renamed, and one instance of another moved out to a
# module call of its own.
module "renamed" {
  source = "./modules/pair"
  name   = "m"
}

moved {
  from = module.m
  to   = module.renamed
}

module "many" {
  source = "./modules/pair"
  count  = 1
  name   = "many-${count.index}"
}

module "split" {
  source = "./modules/pair"
  name   = "many-1"
}

moved {
  from = module.many[1]
  to   = module.split
}
