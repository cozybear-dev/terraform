resource "fp_thing" "stubborn" {
  name   = "stubborn-1"
  value  = "can't be destroyed"
  behave = "fail_destroy"
  lifecycle {
    create_before_destroy = true
  }
}

resource "fp_thing" "half" {
  name   = "half"
  value  = "saved, then reported as failed"
  behave = "fail_partial"
}

resource "fp_thing" "fine" {
  name  = "fine"
  value = "ordinary"
}
