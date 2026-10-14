import { createBrowserRouter } from "react-router";

import { NotFound } from "@/app/NotFound";
import { Shell } from "@/app/Shell";
import { InboxIcon } from "@/components/icons";

import type { ScreenEntry } from "@/app/Rail";

/** The screens the rail lists and the palette and `G` chords reach; each screen registers itself here. */
export const screens: ScreenEntry[] = [
  { key: "o", label: "Overview", path: "/", group: 0, icon: InboxIcon },
];

/** The application's routes under the `/ui` base. */
export const router = createBrowserRouter(
  [
    {
      path: "/",
      element: <Shell screens={screens} />,
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
        { path: "*", element: <NotFound /> },
      ],
    },
  ],
  { basename: "/ui" },
);
