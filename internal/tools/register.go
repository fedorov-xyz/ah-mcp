package tools

import "github.com/mark3labs/mcp-go/server"

// All returns every tool, in catalogue order. Exactly these 25 tools are
// exposed; there are no hidden or stub tools.
func (d *Deps) All() []server.ServerTool {
	return []server.ServerTool{
		// System
		d.whoamiTool(),
		d.serverInfoTool(),
		// Products
		d.searchProductsTool(),
		d.getProductTool(),
		d.searchStoresTool(),
		d.lastChanceTool(),
		// Bonus
		d.bonusOffersTool(),
		d.bonusGroupTool(),
		d.personalBonusTool(),
		d.bonusForHabitsTool(),
		d.savingsSummaryTool(),
		// Shopping list (basket)
		d.getShoppingListTool(),
		d.addToShoppingListTool(),
		d.removeFromShoppingListTool(),
		d.clearShoppingListTool(),
		// Cart
		d.getCartTool(),
		d.updateCartItemTool(),
		d.clearCartTool(),
		d.shoppingListToCartTool(),
		d.findDuplicatesTool(),
		// History
		d.frequentItemsTool(),
		d.purchaseHistoryTool(),
		d.getOrdersTool(),
		d.getOrderDetailsTool(),
		d.getReceiptsTool(),
	}
}

// Register adds every tool to s.
func Register(s *server.MCPServer, d *Deps) {
	s.AddTools(d.All()...)
}
