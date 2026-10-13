variable "salt" {
  default = "one"
}

# A chain of twenty, each waiting only for the one before.
resource "fp_thing" "link_0" {
  name  = "link-0"
  value = var.salt
}

resource "fp_thing" "link_1" {
  name  = "link-1"
  value = fp_thing.link_0.late
}

resource "fp_thing" "link_2" {
  name  = "link-2"
  value = fp_thing.link_1.late
}

resource "fp_thing" "link_3" {
  name  = "link-3"
  value = fp_thing.link_2.late
}

resource "fp_thing" "link_4" {
  name  = "link-4"
  value = fp_thing.link_3.late
}

resource "fp_thing" "link_5" {
  name  = "link-5"
  value = fp_thing.link_4.late
}

resource "fp_thing" "link_6" {
  name  = "link-6"
  value = fp_thing.link_5.late
}

resource "fp_thing" "link_7" {
  name  = "link-7"
  value = fp_thing.link_6.late
}

resource "fp_thing" "link_8" {
  name  = "link-8"
  value = fp_thing.link_7.late
}

resource "fp_thing" "link_9" {
  name  = "link-9"
  value = fp_thing.link_8.late
}

resource "fp_thing" "link_10" {
  name  = "link-10"
  value = fp_thing.link_9.late
}

resource "fp_thing" "link_11" {
  name  = "link-11"
  value = fp_thing.link_10.late
}

resource "fp_thing" "link_12" {
  name  = "link-12"
  value = fp_thing.link_11.late
}

resource "fp_thing" "link_13" {
  name  = "link-13"
  value = fp_thing.link_12.late
}

resource "fp_thing" "link_14" {
  name  = "link-14"
  value = fp_thing.link_13.late
}

resource "fp_thing" "link_15" {
  name  = "link-15"
  value = fp_thing.link_14.late
}

resource "fp_thing" "link_16" {
  name  = "link-16"
  value = fp_thing.link_15.late
}

resource "fp_thing" "link_17" {
  name  = "link-17"
  value = fp_thing.link_16.late
}

resource "fp_thing" "link_18" {
  name  = "link-18"
  value = fp_thing.link_17.late
}

resource "fp_thing" "link_19" {
  name  = "link-19"
  value = fp_thing.link_18.late
}

# A diamond: one at the top, many in the middle, one at the bottom that
# waits for all of them.
resource "fp_thing" "top" {
  name   = "top"
  value  = var.salt
  behave = "sleep:150"
}

resource "fp_thing" "middle" {
  count  = 12
  name   = "middle-${count.index}"
  value  = fp_thing.top.late
  behave = count.index % 3 == 0 ? "sleep:120" : null
}

resource "fp_thing" "bottom" {
  name  = "bottom"
  value = join(",", fp_thing.middle[*].late)
}

# Ordered by depends_on alone, across a module boundary.
module "gate" {
  source     = "./modules/gate"
  depends_on = [fp_thing.bottom, fp_thing.link_19]
}

resource "fp_thing" "last" {
  name       = "last"
  value      = "after everything"
  depends_on = [module.gate]
}
