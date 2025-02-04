package web

import (
	"TUM-Live/dao"
	"TUM-Live/model"
	"TUM-Live/tools"
	"TUM-Live/tools/tum"
	"context"
	"github.com/getsentry/sentry-go"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"gorm.io/gorm"
	"net/http"
	"sort"
	"strconv"
)

var VersionTag string

func MainPage(c *gin.Context) {
	tName := sentry.TransactionName("GET /")
	spanMain := sentry.StartSpan(c.Request.Context(), "MainPageHandler", tName)
	defer spanMain.Finish()

	IsFreshInstallation(c)

	indexData := NewIndexDataWithContext(c)
	indexData.LoadCurrentNotifications()
	indexData.SetYearAndTerm(c)
	indexData.LoadSemesters(spanMain)
	indexData.LoadCoursesForRole(c, spanMain)
	indexData.LoadLivestreams(c)
	indexData.LoadPublicCourses()

	_ = templ.ExecuteTemplate(c.Writer, "index.gohtml", indexData)
}

func AboutPage(c *gin.Context) {
	var indexData IndexData
	var tumLiveContext tools.TUMLiveContext
	tumLiveContextQueried, found := c.Get("TUMLiveContext")
	if found {
		tumLiveContext = tumLiveContextQueried.(tools.TUMLiveContext)
		indexData.TUMLiveContext = tumLiveContext
	} else {
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	indexData.VersionTag = VersionTag

	_ = templ.ExecuteTemplate(c.Writer, "about.gohtml", indexData)
}

type IndexData struct {
	VersionTag          string
	TUMLiveContext      tools.TUMLiveContext
	IsUser              bool
	IsAdmin             bool
	IsStudent           bool
	LiveStreams         []CourseStream
	Courses             []model.Course
	PublicCourses       []model.Course
	Semesters           []dao.Semester
	CurrentYear         int
	CurrentTerm         string
	UserName            string
	ServerNotifications []model.ServerNotification
}

func NewIndexData() IndexData {
	return IndexData{
		VersionTag: VersionTag,
	}
}

func NewIndexDataWithContext(c *gin.Context) IndexData {
	indexData := IndexData{
		VersionTag: VersionTag,
	}

	var tumLiveContext tools.TUMLiveContext
	tumLiveContextQueried, found := c.Get("TUMLiveContext")
	if found {
		tumLiveContext = tumLiveContextQueried.(tools.TUMLiveContext)
		indexData.TUMLiveContext = tumLiveContext
	} else {
		log.Warn("could not get TUMLiveContext")
		c.AbortWithStatus(http.StatusInternalServerError)
	}

	return indexData
}

// IsFreshInstallation Checks whether there are users in the database and executes the appropriate template for it
func IsFreshInstallation(c *gin.Context) {
	res, err := dao.AreUsersEmpty(context.Background()) // fresh installation?
	if err != nil {
		_ = templ.ExecuteTemplate(c.Writer, "error.gohtml", nil)
		return
	} else if res {
		_ = templ.ExecuteTemplate(c.Writer, "onboarding.gohtml", NewIndexData())
		return
	}
}

// LoadCurrentNotifications Loads notifications from the database into the IndexData object
func (d *IndexData) LoadCurrentNotifications() {
	if notifications, err := dao.GetCurrentServerNotifications(); err == nil {
		d.ServerNotifications = notifications
	} else if err != gorm.ErrRecordNotFound {
		log.WithError(err).Warn("could not get server notifications")
	}
}

// SetYearAndTerm Sets year and term on the IndexData object from the URL.
// Aborts with 404 if invalid
func (d *IndexData) SetYearAndTerm(c *gin.Context) {
	var year int
	var term string
	var err error
	if c.Param("year") == "" {
		year, term = tum.GetCurrentSemester()
	} else {
		term = c.Param("term")
		year, err = strconv.Atoi(c.Param("year"))
		if err != nil || (term != "W" && term != "S") {
			c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"message": "Bad semester format in url."})
		}
	}

	d.CurrentYear = year
	d.CurrentTerm = term
}

// LoadSemesters Load available Semesters from the database into the IndexData object
func (d *IndexData) LoadSemesters(spanMain *sentry.Span) {
	d.Semesters = dao.GetAvailableSemesters(spanMain.Context())
}

// LoadLivestreams Load non-hidden, currently live streams into the IndexData object.
// LoggedIn streams can only be seen by logged-in users.
// Enrolled streams can only be seen by users which are allowed to.
func (d *IndexData) LoadLivestreams(c *gin.Context) {
	streams, err := dao.GetCurrentLiveNonHidden(context.Background())
	if err != nil {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"message": "Could not load current livestream from database."})
	}

	tumLiveContext := d.TUMLiveContext

	var livestreams []CourseStream

	for _, stream := range streams {
		courseForLiveStream, _ := dao.GetCourseById(context.Background(), stream.CourseID)

		if courseForLiveStream.Visibility == "loggedin" && tumLiveContext.User == nil {
			continue
		}
		if courseForLiveStream.Visibility == "enrolled" {
			if !isUserAllowedToWatchPrivateCourse(courseForLiveStream, tumLiveContext.User) {
				continue
			}
		}
		livestreams = append(livestreams, CourseStream{
			Course: courseForLiveStream,
			Stream: stream,
		})
	}

	d.LiveStreams = livestreams
}

// LoadCoursesForRole Load all courses of user. Distinguishes between admin, lecturer, and normal users.
func (d *IndexData) LoadCoursesForRole(c *gin.Context, spanMain *sentry.Span) {
	var courses []model.Course

	if d.TUMLiveContext.User != nil {
		switch d.TUMLiveContext.User.Role {
		case model.AdminType:
			courses = dao.GetAllCoursesForSemester(d.CurrentYear, d.CurrentTerm, spanMain.Context())
		case model.LecturerType:
			{
				courses = d.TUMLiveContext.User.CoursesForSemester(d.CurrentYear, d.CurrentTerm, spanMain.Context())
				coursesForLecturer, err :=
					dao.GetCourseForLecturerIdByYearAndTerm(c, d.CurrentYear, d.CurrentTerm, d.TUMLiveContext.User.ID)
				if err == nil {
					courses = append(courses, coursesForLecturer...)
				}
			}
		default:
			courses = d.TUMLiveContext.User.CoursesForSemester(d.CurrentYear, d.CurrentTerm, spanMain.Context())
		}
	}

	sortCourses(courses)

	d.Courses = courses
}

// LoadPublicCourses Load public courses of user. Filter courses which are already in IndexData.Courses
func (d *IndexData) LoadPublicCourses() {
	var public []model.Course
	var err error

	if d.TUMLiveContext.User != nil {
		public, err = dao.GetPublicAndLoggedInCourses(d.CurrentYear, d.CurrentTerm)
	} else {
		public, err = dao.GetPublicCourses(d.CurrentYear, d.CurrentTerm)
	}

	if err != nil {
		d.PublicCourses = []model.Course{}
	} else {
		var publicFiltered []model.Course

		for _, c := range public {
			if !tools.CourseListContains(d.Courses, c.ID) {
				publicFiltered = append(publicFiltered, c)
			}
		}

		sortCourses(publicFiltered)

		d.PublicCourses = publicFiltered
	}
}

type CourseStream struct {
	Course model.Course
	Stream model.Stream
}

func isUserAllowedToWatchPrivateCourse(course model.Course, user *model.User) bool {
	if user != nil {
		for _, c := range user.Courses {
			if c.ID == course.ID {
				return true
			}
		}
		return user.Role == model.AdminType || user.ID == course.UserID
	}
	return false
}

func sortCourses(courses []model.Course){
	sort.Slice(courses, func(i, j int) bool {
		return courses[i].CompareTo(courses[j])
	})
}
