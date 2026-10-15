import { createBrowserRouter } from "react-router";

import { NotFound } from "@/app/NotFound";
import { RouteError } from "@/app/RouteError";
import { Shell } from "@/app/Shell";
import { ChartColumnIcon, HistoryIcon, InboxIcon } from "@/components/icons";

import type { ScreenEntry } from "@/app/Rail";

/** The screens the rail lists and the palette and `G` chords reach; each screen registers itself here. */
export const screens: ScreenEntry[] = [
  { key: "o", label: "Overview", path: "/", group: 0, icon: InboxIcon },
  { key: "h", label: "History", path: "/history", group: 0, icon: HistoryIcon },
  { key: "a", label: "Analytics", path: "/analytics", group: 0, icon: ChartColumnIcon },
];

/** The application's routes under the `/ui` base. */
export const router = createBrowserRouter(
  [
    {
      path: "/",
      element: <Shell screens={screens} />,
      children: [
        {
          ErrorBoundary: RouteError,
          children: [
            {
              index: true,
              lazy: {
                Component: async () =>
                  (await import("@/screens/overview/OverviewScreen")).OverviewScreen,
              },
            },
            {
              path: "issues/:identifier",
              lazy: {
                Component: async () => (await import("@/screens/issue/IssueScreen")).IssueScreen,
              },
            },
            {
              path: "history",
              lazy: {
                Component: async () =>
                  (await import("@/screens/history/HistoryScreen")).HistoryScreen,
              },
            },
            {
              path: "analytics",
              lazy: {
                Component: async () =>
                  (await import("@/screens/analytics/AnalyticsScreen")).AnalyticsScreen,
              },
            },
            { path: "*", element: <NotFound /> },
          ],
        },
      ],
    },
  ],
  { basename: "/ui" },
);
