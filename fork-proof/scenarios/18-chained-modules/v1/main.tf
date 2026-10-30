variable "salt" {
  default = "one"
}

module "stage_1" {
  source   = "./modules/stage"
  name     = "stage-1"
  upstream = { start = "origin" }
  width    = 2
  salt     = var.salt
}

module "stage_2" {
  source   = "./modules/stage"
  name     = "stage-2"
  upstream = module.stage_1.values
  width    = 3
  salt     = var.salt
}

module "stage_3" {
  source   = "./modules/stage"
  name     = "stage-3"
  upstream = module.stage_2.values
  width    = 1
  salt     = var.salt
}

module "stage_4" {
  source   = "./modules/stage"
  name     = "stage-4"
  upstream = module.stage_3.values
  width    = 2
  salt     = var.salt
}

module "stage_5" {
  source   = "./modules/stage"
  name     = "stage-5"
  upstream = module.stage_4.values
  width    = 3
  salt     = var.salt
}

module "stage_6" {
  source   = "./modules/stage"
  name     = "stage-6"
  upstream = module.stage_5.values
  width    = 1
  salt     = var.salt
}

module "stage_7" {
  source   = "./modules/stage"
  name     = "stage-7"
  upstream = module.stage_6.values
  width    = 2
  salt     = var.salt
}

module "stage_8" {
  source   = "./modules/stage"
  name     = "stage-8"
  upstream = module.stage_7.values
  width    = 3
  salt     = var.salt
}

module "stage_9" {
  source   = "./modules/stage"
  name     = "stage-9"
  upstream = module.stage_8.values
  width    = 1
  salt     = var.salt
}

module "stage_10" {
  source   = "./modules/stage"
  name     = "stage-10"
  upstream = module.stage_9.values
  width    = 2
  salt     = var.salt
}

module "stage_11" {
  source   = "./modules/stage"
  name     = "stage-11"
  upstream = module.stage_10.values
  width    = 3
  salt     = var.salt
}

module "stage_12" {
  source   = "./modules/stage"
  name     = "stage-12"
  upstream = module.stage_11.values
  width    = 1
  salt     = var.salt
}

module "stage_13" {
  source   = "./modules/stage"
  name     = "stage-13"
  upstream = module.stage_12.values
  width    = 2
  salt     = var.salt
}

module "stage_14" {
  source   = "./modules/stage"
  name     = "stage-14"
  upstream = module.stage_13.values
  width    = 3
  salt     = var.salt
}

output "final" {
  value = module.stage_14.values
}

output "summaries" {
  value = [module.stage_1.summary, module.stage_7.summary, module.stage_14.summary]
}

output "thirds" {
  value = module.stage_14.third
}
