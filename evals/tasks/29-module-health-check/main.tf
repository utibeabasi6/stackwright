module "api" {
  source = "./modules/service"
  name   = "api"
  vpc_id = "vpc-0abc1234"
}
