resource "fp_thing" "a" {
  name   = "a"
  value  = "first"
  num    = 1.5
  flag   = true
  tags   = { env = "dev", team = "core" }
  items  = ["x", "y"]
  labels = ["l1", "l2"]
  obj    = { a = "A", b = 2 }
  rule {
    port = 80
    cidr = "10.0.0.0/8"
  }
  rule {
    port  = 443
    cidr  = "10.0.0.0/8"
    proto = "tcp"
  }
}

resource "fp_thing" "b" {
  name  = "b"
  value = fp_thing.a.late
}

resource "fp_thing" "c" {
  name  = "c"
  value = "going away"
}

output "a_late" {
  value = fp_thing.a.late
}
