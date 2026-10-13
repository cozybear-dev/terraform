resource "fp_thing" "stubborn" {
  name  = "stubborn-2"
  value = "its predecessor can't be destroyed"
  lifecycle {
    create_before_destroy = true
  }
}

resource "fp_thing" "half" {
  name  = "half"
  value = "replaced because it was tainted"
}

resource "fp_thing" "fine" {
  name  = "fine"
  value = "ordinary"
}
