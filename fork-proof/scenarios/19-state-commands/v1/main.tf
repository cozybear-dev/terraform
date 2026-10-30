resource "fp_thing" "one" {
  name   = "one"
  value  = "first"
  secret = "kept out of the output"
}

resource "fp_thing" "several" {
  count = 3
  name  = "several-${count.index}"
  value = fp_thing.one.late
}

module "box" {
  source = "./modules/box"
}

output "one" {
  value = fp_thing.one.id
}
