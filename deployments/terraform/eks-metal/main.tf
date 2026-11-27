# M10 — AWS production reference IaC (Terraform, per the workspace mandate: Terraform,
# NOT Pulumi). This is a VALIDATED reference for the sandbox's node group + data
# store. The authoritative per-env units live in infra-live (Terragrunt) which
# reference pinned infra-modules versions; this in-repo copy is validated with
# `terraform validate` + `tflint` so the shape ships with the app.
#
# HONEST CAVEAT: `terraform apply` needs a real AWS account (.metal instances, RDS)
# not available in this environment — the milestone delivers validated IaC + the
# tested egress/anti-SSRF acceptance criterion, not a live AWS smoke.

terraform {
  required_version = ">= 1.6"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.60"
    }
  }
}

provider "aws" {
  region = var.region
}

# --- Networking (VPC + subnets) ---------------------------------------------------
resource "aws_vpc" "this" {
  cidr_block           = var.vpc_cidr
  enable_dns_support   = true
  enable_dns_hostnames = true
  tags                 = merge(var.tags, { Name = "${var.name}-vpc" })
}

resource "aws_subnet" "private" {
  count             = length(var.private_subnet_cidrs)
  vpc_id            = aws_vpc.this.id
  cidr_block        = var.private_subnet_cidrs[count.index]
  availability_zone = var.availability_zones[count.index]
  tags              = merge(var.tags, { Name = "${var.name}-private-${count.index}" })
}

# --- EKS cluster ------------------------------------------------------------------
resource "aws_iam_role" "cluster" {
  name               = "${var.name}-eks-cluster"
  assume_role_policy = data.aws_iam_policy_document.eks_assume.json
  tags               = var.tags
}

resource "aws_eks_cluster" "this" {
  name     = var.name
  role_arn = aws_iam_role.cluster.arn
  version  = var.kubernetes_version

  vpc_config {
    subnet_ids              = aws_subnet.private[*].id
    endpoint_private_access = true
    endpoint_public_access  = false # private control plane (worker isolation)
  }
  tags = var.tags
}

# --- .metal node group (Firecracker workers need bare-metal / nested virt) ---------
resource "aws_iam_role" "node" {
  name               = "${var.name}-eks-node"
  assume_role_policy = data.aws_iam_policy_document.node_assume.json
  tags               = var.tags
}

# Launch template enforcing IMDSv2 (http_tokens=required) and hop-limit 1 so a pod
# cannot reach the node's IMDS — the single most important host-level anti-SSRF control
# on AWS (defense in depth behind the egress NetworkPolicy).
resource "aws_launch_template" "node" {
  name_prefix = "${var.name}-node-"

  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required" # IMDSv2 only (no unauthenticated IMDSv1)
    http_put_response_hop_limit = 1          # pods (hop 2) cannot reach IMDS
  }
  tags = var.tags
}

resource "aws_eks_node_group" "metal" {
  cluster_name    = aws_eks_cluster.this.name
  node_group_name = "${var.name}-metal"
  node_role_arn   = aws_iam_role.node.arn
  subnet_ids      = aws_subnet.private[*].id
  instance_types  = [var.metal_instance_type] # e.g. "c5.metal" / "m5.metal" (KVM for Firecracker)

  launch_template {
    id      = aws_launch_template.node.id
    version = "$Latest"
  }

  scaling_config {
    desired_size = var.metal_desired_size
    min_size     = var.metal_min_size
    max_size     = var.metal_max_size
  }

  labels = { "loopsandbox.dev/runtime" = "firecracker" }
  tags   = var.tags
}

# --- RDS (PostgreSQL) — its OWN protected unit (prevent_destroy + deletion prot) ---
resource "aws_db_subnet_group" "this" {
  name       = "${var.name}-db"
  subnet_ids = aws_subnet.private[*].id
  tags       = var.tags
}

resource "aws_db_instance" "postgres" {
  identifier                = "${var.name}-pg"
  engine                    = "postgres"
  engine_version            = var.postgres_version
  instance_class            = var.db_instance_class
  allocated_storage         = var.db_allocated_storage
  db_subnet_group_name      = aws_db_subnet_group.this.name
  username                  = var.db_username
  password                  = var.db_password # supplied by reference (never a VCS literal); the value lands in state — protect state via the SSE-KMS S3 backend (infra-live)
  storage_encrypted         = true
  deletion_protection       = true
  backup_retention_period   = 7
  multi_az                  = true
  skip_final_snapshot       = false
  final_snapshot_identifier = "${var.name}-pg-final"
  tags                      = var.tags

  lifecycle {
    prevent_destroy = true # RDS is a protected unit (workspace mandate)
  }
}

data "aws_iam_policy_document" "eks_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["eks.amazonaws.com"]
    }
  }
}

data "aws_iam_policy_document" "node_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ec2.amazonaws.com"]
    }
  }
}
