# Homebrew formula for opencloudcosts — MCP server for cloud pricing.
# This formula is part of the x7even/homebrew-opencloudcosts tap.

class Opencloudcosts < Formula
  desc "MCP server for cloud pricing (AWS, GCP, Azure)"
  homepage "https://github.com/x7even/cloudcostsmcp"
  version "1.2.0"
  license "MIT"

  on_macos do
    on_arm do
      url "https://github.com/x7even/cloudcostsmcp/releases/download/v#{version}/opencloudcosts_#{version}_darwin_arm64.tar.gz"
      sha256 "13773afd42b4b2336ca721d0345dc4434941d0d5334a3524e1b0f163904f41a6"
    end
    on_intel do
      url "https://github.com/x7even/cloudcostsmcp/releases/download/v#{version}/opencloudcosts_#{version}_darwin_amd64.tar.gz"
      sha256 "3b3cbf67d94018b0150099bccba20bd8055680a96c3d05079737bad309c01b2b"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/x7even/cloudcostsmcp/releases/download/v#{version}/opencloudcosts_#{version}_linux_arm64.tar.gz"
      sha256 "3961e186b6eaa9f4ae090dde007b530b8e790eb94cd2c324e83ad20f6eb6e85c"
    end
    on_intel do
      url "https://github.com/x7even/cloudcostsmcp/releases/download/v#{version}/opencloudcosts_#{version}_linux_amd64.tar.gz"
      sha256 "e78856fccf2ea65cceafe217f9a4d62c19374da2a0d70fb9914796d3d76a238f"
    end
  end

  def install
    bin.install "opencloudcosts"
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/opencloudcosts --version")
  end
end
