resource "fp_thing" "uno" {
  name   = "one"
  value  = "first"
  secret = "kept out of the output"
}

resource "fp_thing" "several" {
  count = 2
  name  = "several-${2 - count.index}"
  value = fp_thing.uno.late
}

module "crate" {
  source = "./modules/box"
}

output "one" {
  value = fp_thing.uno.id
}
