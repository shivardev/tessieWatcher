import { z } from 'zod'
import dashboardCatalogJson from './generated/dashboard-catalog.json'

const gridPositionSchema = z.object({ x: z.number(), y: z.number(), w: z.number(), h: z.number() })
const panelSchema = z.object({
  id: z.number(),
  title: z.string(),
  type: z.string(),
  gridPos: gridPositionSchema,
  options: z.record(z.string(), z.unknown()),
  fieldConfig: z.record(z.string(), z.unknown()),
  queries: z.array(z.string()),
})
const dashboardSchema = z.object({
  key: z.string(),
  title: z.string(),
  panels: z.array(panelSchema),
})
export const dashboardCatalog = z.array(dashboardSchema).parse(dashboardCatalogJson)
export type DashboardDefinition = z.infer<typeof dashboardSchema>
export type PanelDefinition = z.infer<typeof panelSchema>
