#define _GNU_SOURCE
#include <stdio.h>
#include <unistd.h>
#include <sys/types.h>
#include <fcntl.h>
#include <string.h>
#include <signal.h>
#include <errno.h>
#include <stdlib.h>
#include <sys/prctl.h>

#ifndef PREFIX
#define PREFIX "/.crio-lxc/"
#endif

#define runtime_path(NAME) PREFIX NAME

const char* syncfifo = runtime_path("syncfifo");
const char* cmdline_path = runtime_path("cmdline.txt");
const char* environ_path = runtime_path("environ");

int writefifo(const char* fifo, const char*msg) {
  int fd;

#ifdef DEBUG
  printf("writing fifo %s\n", fifo);
#endif

  // Open FIFO for write only 
  fd = open(fifo, O_WRONLY); 
  if (fd == -1)
    return -1;

  if (write(fd, msg, strlen(msg)) == -1)
    return -1;
  
  return close(fd);
}

/* reads up to maxlines-1 lines from path into lines */
int readlines(const char* path, char *buf, int buflen, char **lines, int maxlines) {
  FILE *f;
  char *line;
  int n;

#ifdef DEBUG
  printf("reading lines from %s buflen:%d maxlines:%d\n", path, buflen, maxlines);
#endif

  f = fopen(path, "r");
  if(f == NULL)
      return -1;
  
  errno = 0;
  for(n = 0; n < maxlines-1; n++) {
    line = fgets(buf, buflen, f);
    if (line == NULL) 
      break;
    // line gets truncated if it is longer than buflen ?
    lines[n] = strndup(line, strlen(line)-1);
  }
  if (errno != 0)
    return -1;

  if (fclose(f) != 0)
    return -1;

  lines[n] = (char *) NULL;
  return n;
}


// https://pubs.opengroup.org/onlinepubs/000095399/basedefs/xbd_chap08.html
int load_environment(const char* path, char *buf, int buflen) {
  FILE *f;

#ifdef DEBUG
  printf("reading env from %s buflen:%d\n", path, buflen);
#endif

  f = fopen(path, "r");
  if(f == NULL)
      return -1;
  
  char c;

  while(c != EOF) {
    char *value = NULL;

    for(int i = 0; i < buflen; i++) {
      c = getc(f);
      if (c == EOF)  {
        // we should have receive a '\0' before
        buf[i] = '\0';
        break;
      }

      buf[i] = c;
      if (c == '\0') 
        break;
          
      // buffer is full but we did neither receive '\0' nor EOF before
      if (i == buflen-1)
        return E2BIG; 

      // terminate enviornment key
      // the checks above ensure that we are not at the end of the buffer here
      if (value == NULL && c == '=') {
        buf[i] = '\0';
        value = buf + ( i+1 );
      }
    }
    if (errno != 0)
      return -1;

    // 'foo='
    if (value == NULL)
      return EINVAL;
#ifdef DEBUG    
    printf("setenv %s\n", buf);
#endif
    if (setenv(buf, value, 1) != 0) {
      return -1;
    }
  }
  if (errno != 0)
    return -1;

  return fclose(f);
}

int main(int argc, char** argv)
{
  // Buffer for reading arguments and environment variables.
  // There is not a limit per environment variable, but we limit it to 1MiB here
  // https://stackoverflow.com/questions/53842574/max-size-of-environment-variables-in-kubernetes
  // For arguments "Additionally, the limit per string is 32 pages (the kernel constant MAX_ARG_STRLEN), and the maximum number of strings is 0x7FFFFFFF."
  char buf[1024*1024];
  // see 'man 2 execve' 'Limits on size of arguments and environment'
  // ... ARG_MAX constant (either defined in <limits.h> or available at run time using the call sysconf(_SC_ARG_MAX))
  char *args[256]; // > _POSIX_ARG_MAX+1 

  const char* cid;
  char pname[16];

  if (argc != 2) {
    fprintf(stderr, "invalid number of arguments (expected 2 was %d) usage: %s <containerID>\n", argc, argv[0]);
    exit(1);
  }
   cid = argv[1];
   strncpy(pname, cid, sizeof(pname));
   pname[sizeof(pname)-1] = '\0';

  if (readlines(cmdline_path, buf, sizeof(buf), args, sizeof(args)) == -1){
    perror("failed to read cmdline file");
    exit(1);
   }
  
  // environment is already cleared by liblxc
  //environ = NULL;
  if (load_environment(environ_path, buf, sizeof(buf)) == -1){
    perror("failed to read environment file");
    exit(1);
   }

  // The proc name is used to detect that container is created. 
  // On execve the process name is reset to the name of the new executable file
  if (prctl(PR_SET_NAME,pname,NULL,NULL,NULL) == -1) {
    perror("failed to set process name");
  }

  if (writefifo(syncfifo, cid) == -1) {
    perror("failed to write syncfifo");
    exit(1);
  }
      
  execvp(args[0],args);
}