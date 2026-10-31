include $(TOPDIR)/rules.mk

PKG_NAME:=opensocks-minimal
PKG_VERSION:=0.3.5
PKG_RELEASE:=1
PKG_LICENSE:=GPL-3.0-or-later

include $(INCLUDE_DIR)/package.mk

define Package/opensocks-minimal
  SECTION:=net
  CATEGORY:=Network
  SUBMENU:=Proxy
  TITLE:=Download-on-boot minimal OpenSocks launcher
  DEPENDS:=+nftables-json +kmod-nft-tproxy +ca-bundle +uci +wget-ssl +usign
  PROVIDES:=opensocks-engine
endef

define Package/opensocks-minimal/description
  Minimal verified download-on-boot launcher for OpenSocks. Ships no daemon
  binary; the launcher downloads the signed payload into /tmp and restores it
  after a reboot. The Shadowsocks engine is inside that payload.
endef

define Package/opensocks-minimal/install
	$(INSTALL_DIR) $(1)/etc/init.d $(1)/etc/config $(1)/etc/opensocks $(1)/usr/lib/opensocks
	$(INSTALL_BIN) ./files/etc/init.d/opensocks $(1)/etc/init.d/opensocks
	$(INSTALL_DATA) ./files/etc/config/opensocks $(1)/etc/config/opensocks
	$(INSTALL_BIN) ./files/usr/lib/opensocks/launcher.sh $(1)/usr/lib/opensocks/launcher.sh
endef

$(eval $(call BuildPackage,opensocks-minimal))
