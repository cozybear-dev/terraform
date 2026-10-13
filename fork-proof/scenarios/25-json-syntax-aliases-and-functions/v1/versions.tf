terraform {
  required_providers {
    fp = {
      source                = "hashicorp/fp"
      configuration_aliases = [fp.second]
    }
  }
}

provider "fp" {}

provider "fp" {
  alias = "second"
}
