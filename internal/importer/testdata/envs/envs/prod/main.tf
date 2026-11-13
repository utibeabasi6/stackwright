terraform {
  backend "s3" {
    bucket = "acme-state"
    key    = "prod.tfstate"
    region = "eu-west-1"
  }
}
module "network" {
  source = "../../modules/network"
}
