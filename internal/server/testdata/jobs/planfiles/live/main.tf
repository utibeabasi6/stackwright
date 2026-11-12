terraform {
  backend "http" {}
}

module "zip" {
  source = "../modules/zip"
}

resource "local_file" "copy" {
  filename = "${path.module}/copy.zip"
  source   = module.zip.path
}
