terraform {
  backend "s3" {
    bucket = "acme-state"
    key    = "dev.tfstate"
    region = "eu-west-1"
  }
}
module "network" {
  source = "../../modules/network"
}
