resource "fp_thing" "a" {
  name   = "a"
  value  = "second"
  num    = 2.5
  flag   = false
  tags   = { env = "prod", team = "core", extra = "yes" }
  items  = ["x", "y", "z"]
  labels = ["l2", "l3"]
  obj    = { a = "A2", b = 3 }
  rule {
    port = 8080
    cidr = "10.0.0.0/8"
  }
}

resource "fp_thing" "b" {
  name  = "b-renamed"
  value = fp_thing.a.late
}

resource "fp_thing" "d" {
  name  = "d"
  value = fp_thing.b.derived
}

output "a_late" {
  value = fp_thing.a.late
}

output "d_id" {
  value = fp_thing.d.id
}
