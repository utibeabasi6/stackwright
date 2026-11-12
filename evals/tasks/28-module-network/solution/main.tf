module "network" {
  source         = "./modules/network"
  name_prefix    = "prod"
  vpc_cidr_block = "10.30.0.0/16"
  public_subnets = {
    a = { cidr = "10.30.1.0/24", az = "eu-west-1a" }
    b = { cidr = "10.30.2.0/24", az = "eu-west-1b" }
  }
}
