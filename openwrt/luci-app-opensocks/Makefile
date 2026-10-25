include $(TOPDIR)/rules.mk

PKG_NAME:=luci-app-opensocks
PKG_VERSION:=0.3.1
PKG_RELEASE:=1
PKG_LICENSE:=GPL-3.0-or-later

# The shipped app is still the Lua MVC (luasrc/). luci.mk adds +luci-lua-runtime
# automatically when luasrc/ exists, but luci-compat is what provides the Lua
# dispatcher shims 25.12 needs, and the controller proxies to the daemon by
# shelling out to /usr/bin/curl. Both are named explicitly so the package cannot
# be installed without something the app actually calls.
LUCI_DEPENDS:=+(opensocks||opensocks-minimal) +luci-base +luci-compat +curl

include $(TOPDIR)/feeds/luci/luci.mk

# call BuildPackage - OpenWrt buildroot signature
