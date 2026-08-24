import {
  Suspense,
  lazy,
  useEffect,
  useLayoutEffect,
  type ReactNode,
} from "react";
import {
  Navigate,
  Route,
  Routes,
  useLocation,
  useNavigationType,
} from "react-router";
import { SkyStarfield } from "@/components/SkyStarfield";
import { VideoDetailLoading } from "@/components/VideoDetailLoading";
import { CrawlersPageLoading } from "@/admin/CrawlersPageLoading";
import { DrivesPageLoading } from "@/admin/DrivesPageLoading";
import { useAuth } from "@/admin/AuthContext";
import { RequireAuth } from "@/admin/RequireAuth";
import { RequireAdmin } from "@/admin/RequireAdmin";
import { loadVideoDetailPage } from "@/lib/videoDetailRoute";
import { rememberVideoReturnPath, routeToPath } from "@/lib/videoReturnPath";

const HomePage = lazy(() => import("@/pages/HomePage"));
const ListingPage = lazy(() => import("@/pages/ListingPage"));
const ShortsPage = lazy(() => import("@/pages/ShortsPage"));
const UploadPage = lazy(() => import("@/pages/UploadPage"));
const VideoDetailPage = lazy(loadVideoDetailPage);
const SharedVideoPage = lazy(() => import("@/pages/SharedVideoPage"));

const AdminLayout = lazy(() =>
  import("@/admin/AdminLayout").then((module) => ({
    default: module.AdminLayout,
  }))
);
const LoginPage = lazy(() =>
  import("@/admin/LoginPage").then((module) => ({ default: module.LoginPage }))
);
const DrivesPage = lazy(() =>
  import("@/admin/DrivesPage").then((module) => ({ default: module.DrivesPage }))
);
const CrawlersPage = lazy(() =>
  import("@/admin/CrawlersPage").then((module) => ({
    default: module.CrawlersPage,
  }))
);
const VideosPage = lazy(() =>
  import("@/admin/VideosPage").then((module) => ({ default: module.VideosPage }))
);
const TagsPage = lazy(() =>
  import("@/admin/TagsPage").then((module) => ({ default: module.TagsPage }))
);
const SettingsPage = lazy(() =>
  import("@/admin/SettingsPage").then((module) => ({
    default: module.SettingsPage,
  }))
);
const BackupPage = lazy(() =>
  import("@/admin/BackupPage").then((module) => ({ default: module.BackupPage }))
);
const UsersPage = lazy(() =>
  import("@/admin/UsersPage").then((module) => ({ default: module.UsersPage }))
);
const LogsPage = lazy(() =>
  import("@/admin/LogsPage").then((module) => ({ default: module.LogsPage }))
);

function PageSuspense({
  children,
  fallback = null,
}: {
  children: ReactNode;
  fallback?: ReactNode;
}) {
  return <Suspense fallback={fallback}>{children}</Suspense>;
}

function VideoReturnPathRecorder() {
  const location = useLocation();

  useEffect(() => {
    rememberVideoReturnPath(routeToPath(location));
  }, [location.pathname, location.search, location.hash]);

  return null;
}

function VideoDetailRouteFallback() {
  const { isAdmin } = useAuth();
  const navigationType = useNavigationType();

  // The detail component normally owns this scroll reset, but its module may
  // still be loading. Reset before the fallback paints so a click made far down
  // a listing cannot land below the visible skeleton.
  useLayoutEffect(() => {
    if (navigationType !== "POP") {
      window.scrollTo({ top: 0, behavior: "auto" });
    }
  }, [navigationType]);

  return <VideoDetailLoading isAdmin={isAdmin} />;
}

export default function App() {
  return (
    <>
      {/* 星空蓝主题的固定位置星星层，仅在 data-theme="sky" 下可见 */}
      <SkyStarfield />
      <VideoReturnPathRecorder />
      <Routes>
        <Route
          path="/login"
          element={
            <PageSuspense>
              <LoginPage />
            </PageSuspense>
          }
        />

        {/* 一次性分享页公开；具体视频和媒体请求由分享会话单独鉴权。 */}
        <Route
          path="/share"
          element={
            <PageSuspense>
              <SharedVideoPage />
            </PageSuspense>
          }
        />

        {/* 主站需要登录 */}
        <Route
          path="/"
          element={
            <RequireAuth>
              <PageSuspense>
                <HomePage />
              </PageSuspense>
            </RequireAuth>
          }
        />
        <Route
          path="/list"
          element={
            <RequireAuth>
              <PageSuspense>
                <ListingPage />
              </PageSuspense>
            </RequireAuth>
          }
        />
        <Route
          path="/shorts"
          element={
            <RequireAuth>
              <PageSuspense>
                <ShortsPage />
              </PageSuspense>
            </RequireAuth>
          }
        />
        <Route
          path="/upload"
          element={
            <RequireAuth>
              <RequireAdmin>
                <PageSuspense>
                  <UploadPage />
                </PageSuspense>
              </RequireAdmin>
            </RequireAuth>
          }
        />
        <Route
          path="/video/:id"
          element={
            <RequireAuth>
              <PageSuspense fallback={<VideoDetailRouteFallback />}>
                <VideoDetailPage />
              </PageSuspense>
            </RequireAuth>
          }
        />

        {/* 管理后台也需要登录+管理员权限 */}
        <Route
          path="/admin"
          element={
            <RequireAuth>
              <RequireAdmin>
                <PageSuspense>
                  <AdminLayout />
                </PageSuspense>
              </RequireAdmin>
            </RequireAuth>
          }
        >
          <Route index element={<Navigate to="/admin/drives" replace />} />
          <Route
            path="drives"
            element={
              <PageSuspense fallback={<DrivesPageLoading />}>
                <DrivesPage />
              </PageSuspense>
            }
          />
          <Route
            path="crawlers"
            element={
              <PageSuspense fallback={<CrawlersPageLoading />}>
                <CrawlersPage />
              </PageSuspense>
            }
          />
          <Route
            path="videos"
            element={
              <PageSuspense>
                <VideosPage />
              </PageSuspense>
            }
          />
          <Route
            path="tags"
            element={
              <PageSuspense>
                <TagsPage />
              </PageSuspense>
            }
          />
          <Route
            path="settings"
            element={
              <PageSuspense>
                <SettingsPage />
              </PageSuspense>
            }
          />
          <Route path="theme" element={<Navigate to="/admin/drives" replace />} />
          <Route
            path="backup"
            element={
              <PageSuspense>
                <BackupPage />
              </PageSuspense>
            }
          />
          <Route
            path="users"
            element={
              <PageSuspense>
                <UsersPage />
              </PageSuspense>
            }
          />
          <Route
            path="logs"
            element={
              <PageSuspense>
                <LogsPage />
              </PageSuspense>
            }
          />
        </Route>

        <Route path="*" element={<Navigate to="/" replace />} />
      </Routes>
    </>
  );
}
