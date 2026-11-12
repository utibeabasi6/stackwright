terraform {
  backend "http" {}
}

locals {
  names  = toset(["a", "b"])
  prefix = terraform_data.base.output
}

resource "terraform_data" "base" {
  input = "base"
}

resource "terraform_data" "each" {
  for_each = local.names
  input    = "${local.prefix}-${each.key}"
}

module "net" {
  source = "../modules/net"
  count  = 2
  name   = terraform_data.base.output
}

module "app" {
  source     = "../modules/app"
  subnet     = module.net[0].subnet
  depends_on = [module.net]
}

resource "terraform_data" "after" {
  input      = module.app.id
  depends_on = [terraform_data.each]
}

output "app" {
  value = module.app.id
}
