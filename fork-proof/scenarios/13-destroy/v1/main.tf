resource "fp_thing" "foundation" {
  name  = "foundation"
  value = "first in, last out"
}

module "lower" {
  source = "./modules/tier"
  name   = "lower"
  below  = fp_thing.foundation.late
}

module "upper" {
  source = "./modules/tier"
  name   = "upper"
  below  = module.lower.late
}

resource "fp_thing" "roof" {
  name  = "roof"
  value = module.upper.late
}

output "roof" {
  value = fp_thing.roof.id
}
