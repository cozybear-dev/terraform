module "counted" {
  source = "./modules/group"
  count  = 1
  name   = "counted-${count.index}"
}

resource "fp_thing" "top" {
  name  = "top"
  value = "unrelated"
}
