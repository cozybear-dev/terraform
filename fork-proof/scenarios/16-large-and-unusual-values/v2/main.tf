variable "salt" {
  default = "two"
}

locals {
  # About 300 kB, which on its own is more than the fork's threshold for
  # reading and writing a state in parts.
  # range() gives at most 1,024 numbers, so larger sequences are built
  # from two of them.
  big = join("", flatten([for a in range(90) : [for b in range(100) : format("%08d-%s|<&>|", a * 100 + b, var.salt)]]))

  many = flatten([for a in range(15) : [for b in range(100) : a * 100 + b]])

  strings = {
    empty        = ""
    spaces       = "  leading and trailing  "
    quotes       = "she said \"hi\" and left"
    backslashes  = "C:\\path\\to\\thing"
    html         = "<script>alert('&')</script>"
    newlines     = "line one\nline two\r\nline three\ttabbed"
    control      = "bell\u0007 escape\u001b unit\u001f delete\u007f"
    accents      = "café naïve Ångström"
    decomposed   = "cafe\u0301"
    cjk          = "日本語のテキスト"
    emoji        = "😀 👩‍👩‍👧‍👦 🇳🇱"
    separators   = "line\u2028separator paragraph\u2029separator"
    bom          = "\ufeffstarts with a byte order mark"
    interpolated = "$${not} %%{interpolated}"
    json         = jsonencode({ a = [1, 2, { b = null }], "key \"quoted\"" = "<&>" })
    looks_number = "0123"
    looks_bool   = "true"
    looks_null   = "null"
    long_key     = join("", [for i in range(200) : "k"])
  }
}

resource "fp_thing" "big" {
  name  = "big"
  value = local.big
}

resource "fp_thing" "strings" {
  for_each = local.strings
  name     = "string-${each.key}"
  value    = each.value
  tags     = { (each.value == "" ? "empty" : each.value) = each.key }
  items    = [each.value, each.key, each.value]
  labels   = [each.value, each.key]
}

resource "fp_thing" "numbers" {
  for_each = {
    zero     = 0
    negative = -17
    fraction = 0.1
    third    = 1 / 3
    sum      = 0.1 + 0.2
    large    = 123456789012345678901234567890
    small    = 0.000000000000000000001
    exponent = 1e30
    max_int  = 9223372036854775807
    past_int = 9223372036854775808
    past_53  = 9007199254740993
    float    = 1.5e-7
  }
  name = "number-${each.key}"
  num  = each.value
  obj  = { a = tostring(each.value), b = each.value }
  dyn  = each.value
}

resource "fp_thing" "dynamic" {
  for_each = {
    string       = "text"
    number       = 12
    bool         = true
    list         = ["a", "b"]
    mixed        = ["a", 1, true, null]
    empty_tuple  = []
    empty_object = {}
    nested       = { a = [{ b = { c = [1, 2, 3] } }], d = null, e = {} }
    deep         = [[[[[[[[["bottom"]]]]]]]]]
    map_like     = { for i in range(40) : "key${i}" => i }
  }
  name = "dynamic-${each.key}"
  dyn  = each.value
}

resource "fp_thing" "collections" {
  name   = "collections"
  tags   = { for i in local.many : format("tag-%04d", i) => format("value %d <&>", i) }
  items  = [for i in local.many : format("item %d", i)]
  labels = [for i in local.many : format("label-%04d", i)]
  dynamic "rule" {
    for_each = range(60)
    content {
      port  = 1000 + rule.value
      cidr  = "10.${rule.value}.0.0/16"
      proto = rule.value % 2 == 0 ? "tcp" : null
    }
  }
}

output "strings" {
  value = { for k, t in fp_thing.strings : k => t.value }
}

output "numbers" {
  value = { for k, t in fp_thing.numbers : k => t.num }
}

output "dynamic" {
  value = { for k, t in fp_thing.dynamic : k => t.dyn }
}

output "big_length" {
  value = length(fp_thing.big.value)
}
